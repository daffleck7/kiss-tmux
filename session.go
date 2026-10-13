package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
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

// Session is one terminal: a local process, or an SSH attachment to a remote cs session.
type Session struct {
	ID         string
	Name       string
	Cmd        string
	Cwd        string
	Host       string // "" for local; immutable after creation
	RemoteName string // cs/screen name for remote sessions; immutable
	Running    bool
	State      string
	cpty       ptyProc
	buf        *RingBuffer
	mu         sync.Mutex

	cols, rows          uint16
	disconnectRequested bool
	endRequested        bool
	removed             bool
	connectedAt         time.Time
	backoff             *Backoff
	reconnectGen        int
}

// Manager owns all sessions and their lifecycles.
type Manager struct {
	sessions    sync.Map
	order       []string
	orderMu     sync.Mutex
	broadcast   func([]byte)
	maxSessions int

	hosts      map[string]HostConfig
	startPty   ptyStarter
	runSSH     sshRunner
	sleep      func(time.Duration)
	remoteMu   sync.Mutex // serializes find-then-create of remote sessions
	discoverMu sync.Mutex

	hostErrMu sync.Mutex
	hostErr   map[string]string // last discovery error per host, replayed to new clients
}

// NewManager creates a Manager allowing maxSessions per machine.
func NewManager(maxSessions int, broadcast func([]byte)) *Manager {
	return &Manager{
		broadcast:   broadcast,
		maxSessions: maxSessions,
		hosts:       map[string]HostConfig{},
		hostErr:     map[string]string{},
		startPty:    startConPty,
		runSSH:      runSSHCommand,
		sleep:       time.Sleep,
	}
}

// SetHosts registers the remote machines from config. Call before serving.
func (m *Manager) SetHosts(hosts []HostConfig) {
	m.hosts = make(map[string]HostConfig, len(hosts))
	for _, h := range hosts {
		m.hosts[h.ID] = h
	}
}

func (m *Manager) get(id string) *Session {
	v, ok := m.sessions.Load(id)
	if !ok {
		return nil
	}
	return v.(*Session)
}

func (m *Manager) store(s *Session) {
	m.sessions.Store(s.ID, s)
	m.orderMu.Lock()
	m.order = append(m.order, s.ID)
	m.orderMu.Unlock()
}

func (m *Manager) hostSessionCount(host string) int {
	count := 0
	m.sessions.Range(func(_, v interface{}) bool {
		if v.(*Session).Host == host {
			count++
		}
		return true
	})
	return count
}

// Spawn starts a local session running cmd.
func (m *Manager) Spawn(cmd, cwd, name string, cols, rows uint16) (string, error) {
	if m.hostSessionCount("") >= m.maxSessions {
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

	p, err := m.startPty(cmd, cwd, cols, rows)
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
		State:   stateRunning,
		cpty:    p,
		buf:     NewRingBuffer(65536),
		cols:    cols,
		rows:    rows,
	}
	m.store(s)

	go m.readLoop(s, p)

	m.broadcastSessions()
	return id, nil
}

// readLoop pumps output from p until it closes. p is passed explicitly because a
// remote session swaps in a new PTY on reconnect.
func (m *Manager) readLoop(s *Session, p ptyProc) {
	buf := make([]byte, 4096)
	for {
		n, err := p.Read(buf)
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
	m.onPtyEnd(s)
}

// onPtyEnd moves a session to its post-exit state and starts reconnecting if needed.
func (m *Manager) onPtyEnd(s *Session) {
	s.mu.Lock()
	s.Running = false
	if s.removed {
		s.mu.Unlock()
		return
	}
	next := stateAfterExit(s.Host != "", s.disconnectRequested, s.endRequested)
	s.State = next
	gen := 0
	if next == stateReconnecting {
		if time.Since(s.connectedAt) >= stableConnection {
			s.backoff.Reset()
		}
		s.reconnectGen++
		gen = s.reconnectGen
	}
	s.mu.Unlock()

	if s.Host == "" {
		exitMsg := ServerMsg{Type: "exited", Session: s.ID, Code: 0}
		if encoded, err := json.Marshal(exitMsg); err == nil {
			m.broadcast(encoded)
		}
	}
	m.broadcastSessions()

	if next == stateReconnecting {
		go m.reconnectLoop(s, gen, false)
	}
}

// Input writes keyboard data to a running session in small chunks.
func (m *Manager) Input(id, data string) error {
	s := m.get(id)
	if s == nil {
		return errors.New("session not found")
	}
	s.mu.Lock()
	running := s.Running
	p := s.cpty
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
		if _, err := p.Write(b[:n]); err != nil {
			return err
		}
		b = b[n:]
		if len(b) > 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return nil
}

// Resize records the terminal size and applies it if the session is running.
func (m *Manager) Resize(id string, cols, rows uint16) error {
	s := m.get(id)
	if s == nil {
		return errors.New("session not found")
	}
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	running := s.Running
	p := s.cpty
	s.mu.Unlock()
	if !running {
		return nil
	}
	return p.Resize(int(cols), int(rows))
}

// Kill stops a local session. For remote sessions it only disconnects.
func (m *Manager) Kill(id string) {
	s := m.get(id)
	if s == nil {
		return
	}
	if s.Host != "" {
		m.Disconnect(id)
		return
	}
	s.mu.Lock()
	wasRunning := s.Running
	s.Running = false
	s.State = stateExited
	p := s.cpty
	s.mu.Unlock()

	if wasRunning {
		p.Close()
	}
}

// Remove stops a session (remote: disconnects only) and drops it from the list.
func (m *Manager) Remove(id string) {
	s := m.get(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	wasRunning := s.Running
	s.Running = false
	s.removed = true
	s.reconnectGen++
	p := s.cpty
	s.mu.Unlock()

	if wasRunning {
		p.Close()
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

// Rename changes a session's display name.
func (m *Manager) Rename(id, name string) {
	s := m.get(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Name = name
	s.mu.Unlock()
	m.broadcastSessions()
}

// List returns all sessions in creation order.
func (m *Manager) List() []SessionInfo {
	m.orderMu.Lock()
	order := make([]string, len(m.order))
	copy(order, m.order)
	m.orderMu.Unlock()

	var result []SessionInfo
	for _, id := range order {
		s := m.get(id)
		if s == nil {
			continue
		}
		s.mu.Lock()
		info := SessionInfo{
			ID:         s.ID,
			Name:       s.Name,
			Cmd:        s.Cmd,
			Running:    s.Running,
			Host:       s.Host,
			State:      s.State,
			RemoteName: s.RemoteName,
		}
		s.mu.Unlock()
		result = append(result, info)
	}
	return result
}

// Replay returns the buffered output for a session.
func (m *Manager) Replay(id string) []byte {
	s := m.get(id)
	if s == nil {
		return nil
	}
	return s.buf.Bytes()
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

// Sentinel errors from spawnRemoteSession, wrapped with context.
var (
	errSessionOpen = errors.New("session already open")
	errMaxSessions = errors.New("max sessions reached")
)

// SpawnRemote opens a new cs session called name (slugified) on hostID in cwd.
func (m *Manager) SpawnRemote(hostID, name, cwd string, cols, rows uint16) (string, error) {
	host, ok := m.hosts[hostID]
	if !ok {
		return "", fmt.Errorf("unknown host %q", hostID)
	}
	displayName := strings.TrimSpace(name)
	if strings.TrimSpace(cwd) == "" {
		cwd = host.DefaultCwd
	}
	return m.spawnRemoteSession(host, slugRemoteName(name), displayName, cwd, cols, rows)
}

// spawnRemoteSession attaches to (or creates) remoteName on host and registers it.
// An empty remoteName gets a generated one.
func (m *Manager) spawnRemoteSession(host HostConfig, remoteName, displayName, cwd string, cols, rows uint16) (string, error) {
	m.remoteMu.Lock()
	defer m.remoteMu.Unlock()

	if m.hostSessionCount(host.ID) >= m.maxSessions {
		return "", fmt.Errorf("%w on %s", errMaxSessions, host.Label)
	}
	id := genID()
	if remoteName == "" {
		remoteName = "s" + id
	}
	if !validRemoteName(remoteName) {
		return "", fmt.Errorf("invalid session name %q (use letters, numbers, - or _)", remoteName)
	}
	if m.findRemote(host.ID, remoteName) != nil {
		return "", fmt.Errorf("%w: %q on %s", errSessionOpen, remoteName, host.Label)
	}
	if displayName == "" {
		displayName = remoteName
	}
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}

	s := &Session{
		ID:         id,
		Name:       displayName,
		Cmd:        "cs " + remoteName,
		Cwd:        cwd,
		Host:       host.ID,
		RemoteName: remoteName,
		State:      stateReconnecting,
		buf:        NewRingBuffer(65536),
		cols:       cols,
		rows:       rows,
		backoff:    newBackoff(),
	}
	if err := m.attach(s, host, cwd, 0); err != nil {
		return "", err
	}
	m.store(s)
	m.broadcastSessions()
	return id, nil
}

// findRemote returns the session for (hostID, remoteName), if open.
func (m *Manager) findRemote(hostID, remoteName string) *Session {
	var found *Session
	m.sessions.Range(func(_, v interface{}) bool {
		s := v.(*Session)
		if s.Host == hostID && s.RemoteName == remoteName {
			found = s
			return false
		}
		return true
	})
	return found
}

// attach starts an ssh PTY for s and begins reading. cwd is only used for new sessions.
// gen is the reconnect generation the caller acted on; a stale gen discards the PTY.
func (m *Manager) attach(s *Session, host HostConfig, cwd string, gen int) error {
	s.mu.Lock()
	cols, rows := s.cols, s.rows
	s.mu.Unlock()

	cmdline := windowsCommandLine(sshExecutable(), attachArgs(host.SSH, s.RemoteName, cwd))
	p, err := m.startPty(cmdline, "", cols, rows)
	if err != nil {
		log.Printf("attach %s on %s: %v", s.RemoteName, host.ID, err)
		return fmt.Errorf("attach %s on %s: %w", s.RemoteName, host.Label, err)
	}

	s.mu.Lock()
	if s.removed || s.reconnectGen != gen {
		s.mu.Unlock()
		p.Close()
		return nil
	}
	s.cpty = p
	s.Running = true
	s.State = stateRunning
	s.connectedAt = time.Now()
	s.mu.Unlock()

	go m.readLoop(s, p)
	return nil
}

// broadcastError sends an error to all clients, tagged with host and session.
func (m *Manager) broadcastError(hostID, sessionID, text string) {
	msg := ServerMsg{Type: "error", Host: hostID, Session: sessionID, Error: text}
	if encoded, err := json.Marshal(msg); err == nil {
		m.broadcast(encoded)
	}
}

// listRemote returns cs session names on host. A non-zero exit from screen itself
// is normal (screen -ls exits 1 with or without sessions); only ssh failures are errors.
func (m *Manager) listRemote(host HostConfig) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sshTimeout)
	defer cancel()
	out, err := m.runSSH(ctx, listArgs(host.SSH))
	if err != nil && !isRemoteExitError(err) {
		return nil, fmt.Errorf("list sessions on %s: %w", host.Label, err)
	}
	names, skipped := parseScreenList(string(out))
	for _, name := range skipped {
		log.Printf("list %s: ignoring session %q: invalid name", host.ID, remoteSessionPrefix+name)
	}
	return names, nil
}

// reconnectLoop reattaches s until it succeeds, the session is gone on the remote,
// or the loop is superseded (gen changed) or cancelled by the user.
func (m *Manager) reconnectLoop(s *Session, gen int, immediate bool) {
	host, ok := m.hosts[s.Host]
	if !ok {
		m.setState(s, gen, stateDisconnected)
		return
	}
	for {
		// Checked first so a superseded loop never advances the shared backoff.
		if m.reconnectCancelled(s, gen) {
			return
		}
		if !immediate {
			s.mu.Lock()
			delay := s.backoff.Next()
			s.mu.Unlock()
			m.sleep(delay)
		}
		immediate = false
		if m.reconnectCancelled(s, gen) {
			return
		}

		names, err := m.listRemote(host)
		switch nextReconnectAction(err, names, s.RemoteName) {
		case actionEnded:
			m.setState(s, gen, stateEnded)
			return
		case actionRetry:
			log.Printf("reconnect %s on %s: %v", s.RemoteName, host.ID, err)
			continue
		}

		if m.reconnectCancelled(s, gen) {
			return
		}
		// If the session died after `screen -ls`, `cs <name>` creates a fresh one in the
		// default folder. Accepted; a real fix needs an attach-only mode in cs.
		if err := m.attach(s, host, "", gen); err != nil {
			continue
		}
		m.broadcastSessions()
		return
	}
}

// reconnectCancelled reports whether a reconnect loop of generation gen should stop.
func (m *Manager) reconnectCancelled(s *Session, gen int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removed || s.endRequested || s.reconnectGen != gen || s.State != stateReconnecting
}

// setState sets state if the reconnect generation is still current, then broadcasts.
func (m *Manager) setState(s *Session, gen int, state string) {
	s.mu.Lock()
	if s.reconnectGen != gen || s.removed {
		s.mu.Unlock()
		return
	}
	s.State = state
	s.mu.Unlock()
	m.broadcastSessions()
}

// Disconnect closes the SSH link of a remote session; Claude keeps running remotely.
func (m *Manager) Disconnect(id string) {
	s := m.get(id)
	if s == nil || s.Host == "" {
		return
	}
	s.mu.Lock()
	s.disconnectRequested = true
	s.reconnectGen++
	running := s.Running
	p := s.cpty
	if !running {
		s.State = stateDisconnected
	}
	s.mu.Unlock()

	if running {
		p.Close()
		return
	}
	m.broadcastSessions()
}

// Reconnect immediately reattaches a disconnected (or ended) remote session.
func (m *Manager) Reconnect(id string) {
	s := m.get(id)
	if s == nil || s.Host == "" {
		return
	}
	s.mu.Lock()
	if s.Running || s.State == stateReconnecting {
		s.mu.Unlock()
		return
	}
	s.disconnectRequested = false
	s.endRequested = false
	s.State = stateReconnecting
	s.backoff.Reset()
	s.reconnectGen++
	gen := s.reconnectGen
	s.mu.Unlock()

	m.broadcastSessions()
	go m.reconnectLoop(s, gen, true)
}

// End stops the cs session on the remote machine (cs -k) and closes the link.
// screen matches session names by prefix, so cs -k only runs when the exact name
// is listed; otherwise `cs -k api` could kill cs-api2.
func (m *Manager) End(id string) {
	s := m.get(id)
	if s == nil || s.Host == "" {
		return
	}
	host, ok := m.hosts[s.Host]
	if !ok {
		return
	}
	s.mu.Lock()
	s.endRequested = true
	s.mu.Unlock()

	go func() {
		names, err := m.listRemote(host)
		if err != nil {
			log.Printf("end %s on %s: %v", s.RemoteName, host.ID, err)
			m.endFailed(s, host, err)
			return
		}
		if containsName(names, s.RemoteName) {
			ctx, cancel := context.WithTimeout(context.Background(), sshTimeout)
			defer cancel()
			if _, err := m.runSSH(ctx, endArgs(host.SSH, s.RemoteName)); err != nil {
				log.Printf("end %s on %s: %v", s.RemoteName, host.ID, err)
				m.endFailed(s, host, err)
				return
			}
		} else {
			log.Printf("end %s on %s: not listed, marking ended", s.RemoteName, host.ID)
		}
		m.markEnded(s)
	}()
}

// endFailed undoes an End request that could not be carried out and reports it.
func (m *Manager) endFailed(s *Session, host HostConfig, err error) {
	s.mu.Lock()
	s.endRequested = false
	// The link may have dropped while End was in flight, which marked the session
	// ended (or left it reconnecting with its loop stopped by endRequested). The end
	// failed, so the session may still exist: show disconnected.
	revert := !s.Running && !s.removed &&
		(s.State == stateEnded || s.State == stateReconnecting)
	if revert {
		s.State = stateDisconnected
		s.reconnectGen++
	}
	s.mu.Unlock()
	m.broadcastError(host.ID, s.ID, fmt.Sprintf("Could not end %s on %s: %v", s.RemoteName, host.Label, err))
	if revert {
		m.broadcastSessions()
	}
}

// markEnded stops any reconnect and closes the link of a session that no longer exists.
func (m *Manager) markEnded(s *Session) {
	s.mu.Lock()
	s.reconnectGen++
	running := s.Running
	p := s.cpty
	if !running {
		s.State = stateEnded
	}
	s.mu.Unlock()
	if running {
		p.Close()
		return
	}
	m.broadcastSessions()
}

// Discover attaches to any cs sessions on hostID that are not already open.
func (m *Manager) Discover(hostID string) {
	host, ok := m.hosts[hostID]
	if !ok {
		return
	}
	m.discoverMu.Lock()
	defer m.discoverMu.Unlock()

	names, err := m.listRemote(host)
	if err != nil {
		log.Printf("discover %s: %v", host.ID, err)
		text := fmt.Sprintf("Can't reach %s: %v", host.Label, err)
		m.setHostError(host.ID, text)
		m.broadcastError(host.ID, "", text)
		return
	}
	m.setHostError(host.ID, "")
	if encoded, err := json.Marshal(ServerMsg{Type: "host_ok", Host: host.ID}); err == nil {
		m.broadcast(encoded)
	}
	for _, name := range names {
		if m.findRemote(host.ID, name) != nil {
			continue
		}
		// Attach without a folder. If the session died after `screen -ls`, cs creates a
		// fresh one in the default folder; accepted (a real fix needs attach-only cs).
		_, err := m.spawnRemoteSession(host, name, name, "", 80, 24)
		switch {
		case err == nil, errors.Is(err, errSessionOpen):
			continue
		case errors.Is(err, errMaxSessions):
			log.Printf("discover %s: adopt %s: %v", host.ID, name, err)
			m.broadcastError(host.ID, "", fmt.Sprintf("Couldn't open %s: %v", name, err))
			return
		default:
			log.Printf("discover %s: adopt %s: %v", host.ID, name, err)
			m.broadcastError(host.ID, "", fmt.Sprintf("Couldn't open %s on %s: %v", name, host.Label, err))
		}
	}
}

// setHostError records the last discovery error for hostID; "" clears it.
func (m *Manager) setHostError(hostID, text string) {
	m.hostErrMu.Lock()
	defer m.hostErrMu.Unlock()
	if text == "" {
		delete(m.hostErr, hostID)
		return
	}
	m.hostErr[hostID] = text
}

// HostErrors returns a copy of the last discovery error per host.
func (m *Manager) HostErrors() map[string]string {
	m.hostErrMu.Lock()
	defer m.hostErrMu.Unlock()
	out := make(map[string]string, len(m.hostErr))
	for host, text := range m.hostErr {
		out[host] = text
	}
	return out
}

// DiscoverAll runs Discover for every configured host.
func (m *Manager) DiscoverAll() {
	for id := range m.hosts {
		m.Discover(id)
	}
}

// BrowseRemote lists subdirectories of p on hostID (host default when p is empty).
func (m *Manager) BrowseRemote(hostID, p string) (BrowseResult, error) {
	host, ok := m.hosts[hostID]
	if !ok {
		return BrowseResult{}, fmt.Errorf("unknown host %q", hostID)
	}
	if p == "" {
		p = host.DefaultCwd
	}
	ctx, cancel := context.WithTimeout(context.Background(), sshTimeout)
	defer cancel()
	out, err := m.runSSH(ctx, batchArgs(host.SSH, browseRemoteCommand(p)))
	if err != nil {
		return BrowseResult{}, fmt.Errorf("browse %s on %s: %w", p, host.Label, err)
	}
	abs, dirs, err := parseBrowseOutput(string(out))
	if err != nil {
		return BrowseResult{}, fmt.Errorf("browse %s on %s: %w", p, host.Label, err)
	}
	entries := make([]DirEntry, 0, len(dirs))
	for _, d := range dirs {
		entries = append(entries, DirEntry{Name: d, IsDir: true})
	}
	return BrowseResult{
		Path:    abs,
		Parent:  remoteParent(abs),
		Entries: entries,
		Drives:  []string{},
		Sep:     "/",
	}, nil
}
