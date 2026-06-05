package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/UserExistsError/conpty"
)

// RingBuffer is a fixed-size circular byte buffer for reconnect replay.
type RingBuffer struct {
	buf  []byte
	size int
	w    int
	full bool
	mu   sync.Mutex
}

// NewRingBuffer creates a ring buffer with the given capacity in bytes.
func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		buf:  make([]byte, size),
		size: size,
	}
}

// Write appends data to the ring buffer, overwriting oldest data if full.
func (rb *RingBuffer) Write(p []byte) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	for len(p) > 0 {
		// If write is larger than entire buffer, skip to last buffer-sized chunk
		if len(p) >= rb.size {
			copy(rb.buf, p[len(p)-rb.size:])
			rb.w = 0
			rb.full = true
			return
		}

		n := copy(rb.buf[rb.w:], p)
		p = p[n:]
		rb.w += n
		if rb.w >= rb.size {
			rb.w = 0
			rb.full = true
		}
	}
}

// Bytes returns all data in the buffer in chronological order.
func (rb *RingBuffer) Bytes() []byte {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if !rb.full {
		out := make([]byte, rb.w)
		copy(out, rb.buf[:rb.w])
		return out
	}

	out := make([]byte, rb.size)
	n := copy(out, rb.buf[rb.w:])
	copy(out[n:], rb.buf[:rb.w])
	return out
}

func genID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type Session struct {
	ID      string
	Name    string
	Cmd     string
	Cwd     string
	Running bool
	cpty    *conpty.ConPty
	buf     *RingBuffer
	mu      sync.Mutex
}

type Manager struct {
	sessions    sync.Map
	order       []string
	orderMu     sync.Mutex
	broadcast   func([]byte)
	maxSessions int
}

func NewManager(maxSessions int, broadcast func([]byte)) *Manager {
	return &Manager{
		broadcast:   broadcast,
		maxSessions: maxSessions,
	}
}

func (m *Manager) sessionCount() int {
	count := 0
	m.sessions.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

func (m *Manager) Spawn(cmd, cwd, name string, cols, rows uint16) (string, error) {
	if m.sessionCount() >= m.maxSessions {
		return "", errors.New("max sessions reached")
	}

	if cmd == "" {
		cmd = "claude --dangerously-skip-permissions"
	}
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}

	opts := []conpty.ConPtyOption{
		conpty.ConPtyDimensions(int(cols), int(rows)),
	}
	if cwd != "" {
		opts = append(opts, conpty.ConPtyWorkDir(cwd))
	}

	cpty, err := conpty.Start(cmd, opts...)
	if err != nil {
		return "", err
	}

	id := genID()
	if name == "" {
		name = cmd
	}

	s := &Session{
		ID:      id,
		Name:    name,
		Cmd:     cmd,
		Cwd:     cwd,
		Running: true,
		cpty:    cpty,
		buf:     NewRingBuffer(65536),
	}

	m.sessions.Store(id, s)
	m.orderMu.Lock()
	m.order = append(m.order, id)
	m.orderMu.Unlock()

	go m.readLoop(s)

	m.broadcastSessions()
	return id, nil
}

func (m *Manager) readLoop(s *Session) {
	buf := make([]byte, 4096)
	for {
		n, err := s.cpty.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])

			s.buf.Write(data)

			msg := ServerMsg{Type: "output", Session: s.ID, Data: string(data)}
			if encoded, err := json.Marshal(msg); err == nil {
				m.broadcast(encoded)
			}
		}
		if err != nil {
			break
		}
	}

	s.mu.Lock()
	s.Running = false
	s.mu.Unlock()

	exitMsg := ServerMsg{Type: "exited", Session: s.ID, Code: 0}
	if encoded, err := json.Marshal(exitMsg); err == nil {
		m.broadcast(encoded)
	}
	m.broadcastSessions()
}

func (m *Manager) Input(id, data string) error {
	v, ok := m.sessions.Load(id)
	if !ok {
		return errors.New("session not found")
	}
	s := v.(*Session)
	s.mu.Lock()
	running := s.Running
	s.mu.Unlock()
	if !running {
		return errors.New("session not running")
	}

	b := []byte(data)
	const chunkSize = 256
	for len(b) > 0 {
		n := chunkSize
		if n > len(b) {
			n = len(b)
		}
		if _, err := s.cpty.Write(b[:n]); err != nil {
			return err
		}
		b = b[n:]
		if len(b) > 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return nil
}

func (m *Manager) Resize(id string, cols, rows uint16) error {
	v, ok := m.sessions.Load(id)
	if !ok {
		return errors.New("session not found")
	}
	s := v.(*Session)
	s.mu.Lock()
	running := s.Running
	s.mu.Unlock()
	if !running {
		return nil
	}
	return s.cpty.Resize(int(cols), int(rows))
}

func (m *Manager) Kill(id string) {
	v, ok := m.sessions.Load(id)
	if !ok {
		return
	}
	s := v.(*Session)
	s.mu.Lock()
	wasRunning := s.Running
	s.Running = false
	s.mu.Unlock()

	if wasRunning {
		s.cpty.Close()
	}
}

func (m *Manager) Remove(id string) {
	v, ok := m.sessions.Load(id)
	if !ok {
		return
	}
	s := v.(*Session)
	s.mu.Lock()
	wasRunning := s.Running
	s.Running = false
	s.mu.Unlock()

	if wasRunning {
		s.cpty.Close()
	}

	m.sessions.Delete(id)

	m.orderMu.Lock()
	for i, oid := range m.order {
		if oid == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.orderMu.Unlock()

	m.broadcastSessions()
}

func (m *Manager) Rename(id, name string) {
	v, ok := m.sessions.Load(id)
	if !ok {
		return
	}
	s := v.(*Session)
	s.mu.Lock()
	s.Name = name
	s.mu.Unlock()
	m.broadcastSessions()
}

func (m *Manager) List() []SessionInfo {
	m.orderMu.Lock()
	order := make([]string, len(m.order))
	copy(order, m.order)
	m.orderMu.Unlock()

	var result []SessionInfo
	for _, id := range order {
		v, ok := m.sessions.Load(id)
		if !ok {
			continue
		}
		s := v.(*Session)
		s.mu.Lock()
		info := SessionInfo{
			ID:      s.ID,
			Name:    s.Name,
			Cmd:     s.Cmd,
			Running: s.Running,
		}
		s.mu.Unlock()
		result = append(result, info)
	}
	return result
}

func (m *Manager) Replay(id string) []byte {
	v, ok := m.sessions.Load(id)
	if !ok {
		return nil
	}
	return v.(*Session).buf.Bytes()
}

func (m *Manager) SessionsMessage() ServerMsg {
	return ServerMsg{Type: "sessions", Sessions: m.List()}
}

func (m *Manager) broadcastSessions() {
	msg := m.SessionsMessage()
	if encoded, err := json.Marshal(msg); err == nil {
		m.broadcast(encoded)
	}
}
