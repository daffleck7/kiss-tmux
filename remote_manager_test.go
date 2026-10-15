package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePty is an in-memory ptyProc. Read blocks until Close.
type fakePty struct {
	out    chan []byte
	closed chan struct{}
	once   sync.Once
}

func newFakePty() *fakePty {
	return &fakePty{out: make(chan []byte, 16), closed: make(chan struct{})}
}

func (f *fakePty) Read(p []byte) (int, error) {
	select {
	case b := <-f.out:
		return copy(p, b), nil
	case <-f.closed:
		return 0, io.EOF
	}
}

func (f *fakePty) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakePty) Resize(int, int) error       { return nil }
func (f *fakePty) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// fakeEnv records PTY starts and scripts SSH responses.
type fakeEnv struct {
	mu       sync.Mutex
	started  []string
	ptys     []*fakePty
	sshCalls [][]string
	// listResults are consumed in order by `cs --ls` calls; the last one repeats.
	listResults []listResult
	endErr      error
	browseOut   string
	sleeps      []time.Duration

	// endGate, when non-nil, blocks `cs -k` calls until it is closed.
	endGate chan struct{}
	// listGate, when non-nil, blocks `cs --ls` calls until it is closed;
	// listBlocked (if non-nil) is closed when the first call starts waiting.
	listGate    chan struct{}
	listBlocked chan struct{}
	// startGate, when non-nil, blocks the second startPty call until it is closed;
	// startBlocked (if non-nil) is closed when that call starts waiting.
	startGate    chan struct{}
	startBlocked chan struct{}
	// startErrFor makes startPty fail when the command line contains it.
	startErrFor string
}

type listResult struct {
	out string
	err error
}

func (e *fakeEnv) startPty(cmdline, cwd string, cols, rows uint16) (ptyProc, error) {
	e.mu.Lock()
	gate, blocked := e.startGate, e.startBlocked
	second := len(e.started) == 1
	if second {
		e.startGate, e.startBlocked = nil, nil
	}
	errFor := e.startErrFor
	e.mu.Unlock()
	if errFor != "" && strings.Contains(cmdline, errFor) {
		return nil, errors.New("start failed for " + errFor)
	}
	if second && gate != nil {
		if blocked != nil {
			close(blocked)
		}
		<-gate
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	p := newFakePty()
	e.started = append(e.started, cmdline)
	e.ptys = append(e.ptys, p)
	return p, nil
}

func (e *fakeEnv) runSSH(ctx context.Context, args []string) ([]byte, error) {
	remote := args[len(args)-1]
	e.mu.Lock()
	e.sshCalls = append(e.sshCalls, args)
	var gate chan struct{}
	switch {
	case remote == "cs --ls":
		gate = e.listGate
		if gate != nil && e.listBlocked != nil {
			close(e.listBlocked)
			e.listBlocked = nil
		}
	case strings.HasPrefix(remote, "cs -k "):
		gate = e.endGate
	}
	e.mu.Unlock()
	if gate != nil {
		<-gate
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case remote == "cs --ls":
		if len(e.listResults) == 0 {
			return nil, nil
		}
		r := e.listResults[0]
		if len(e.listResults) > 1 {
			e.listResults = e.listResults[1:]
		}
		return []byte(r.out), r.err
	case strings.HasPrefix(remote, "cs -k "):
		return nil, e.endErr
	case strings.HasPrefix(remote, "cd -- "):
		return []byte(e.browseOut), nil
	}
	return nil, errors.New("unexpected ssh call: " + remote)
}

func (e *fakeEnv) sleep(d time.Duration) {
	e.mu.Lock()
	e.sleeps = append(e.sleeps, d)
	e.mu.Unlock()
}

func (e *fakeEnv) pty(i int) *fakePty {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ptys[i]
}

func (e *fakeEnv) startCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.started)
}

func newRemoteTestManager(t *testing.T) (*Manager, *fakeEnv) {
	t.Helper()
	env := &fakeEnv{}
	m := NewManager(8, func([]byte) {})
	m.startPty = env.startPty
	m.runSSH = env.runSSH
	m.sleep = env.sleep
	m.SetHosts([]HostConfig{{ID: "mac", Label: "Mac", SSH: "mac", DefaultCwd: "~"}})
	return m, env
}

func screenLs(names ...string) string {
	var b strings.Builder
	b.WriteString("There are screens on:\n")
	for i, n := range names {
		b.WriteString("\t" + string(rune('1'+i)) + "000.cs-" + n + "\t(10/13/26)\t(Detached)\n")
	}
	return b.String()
}

func sessionState(m *Manager, id string) string {
	for _, s := range m.List() {
		if s.ID == id {
			return s.State
		}
	}
	return "missing"
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSpawnRemoteStartsSSH(t *testing.T) {
	m, env := newRemoteTestManager(t)
	id, err := m.SpawnRemote("mac", "My App", "~/Desktop/My Projects", "claude -c", 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if env.startCount() != 1 {
		t.Fatalf("starts = %d, want 1", env.startCount())
	}
	cmdline := env.started[0]
	if !strings.Contains(cmdline, `cs My-App ~/'Desktop/My Projects' 'claude -c'`) {
		t.Errorf("cmdline = %s", cmdline)
	}
	list := m.List()
	if len(list) != 1 || list[0].ID != id || list[0].Host != "mac" ||
		list[0].Name != "My App" || list[0].State != stateRunning {
		t.Errorf("list = %+v", list)
	}
}

func TestSpawnRemoteDefaultsName(t *testing.T) {
	m, env := newRemoteTestManager(t)
	id, err := m.SpawnRemote("mac", "", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.started[0], "cs s"+id) {
		t.Errorf("cmdline = %s, want generated name s%s", env.started[0], id)
	}
}

func TestSpawnRemoteRejectsUnknownHostAndDuplicates(t *testing.T) {
	m, _ := newRemoteTestManager(t)
	if _, err := m.SpawnRemote("nope", "a", "", "", 80, 24); err == nil {
		t.Error("expected unknown host error")
	}
	if _, err := m.SpawnRemote("mac", "a", "", "", 80, 24); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SpawnRemote("mac", "a", "", "", 80, 24); err == nil {
		t.Error("expected duplicate session error")
	}
}

func TestSessionCapIsPerMachine(t *testing.T) {
	m, _ := newRemoteTestManager(t)
	m.maxSessions = 1
	if _, err := m.SpawnRemote("mac", "a", "", "", 80, 24); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SpawnRemote("mac", "b", "", "", 80, 24); err == nil {
		t.Error("expected max sessions error on mac")
	}
	if got := m.hostSessionCount(""); got != 0 {
		t.Errorf("local count = %d, want 0", got)
	}
}

func TestDropReconnectsWhenSessionStillExists(t *testing.T) {
	m, env := newRemoteTestManager(t)
	env.listResults = []listResult{{out: screenLs("main")}}
	id, err := m.SpawnRemote("mac", "main", "", "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	env.pty(0).Close()
	waitFor(t, "second attach", func() bool { return env.startCount() == 2 })
	waitFor(t, "running", func() bool { return sessionState(m, id) == stateRunning })
	if !strings.HasSuffix(env.started[1], `"cs main"`) {
		t.Errorf("reattach cmdline = %s, want no cwd", env.started[1])
	}
	if len(env.sleeps) == 0 || env.sleeps[0] != time.Second {
		t.Errorf("sleeps = %v, want first 1s", env.sleeps)
	}
}

func TestDropMarksEndedWhenSessionGone(t *testing.T) {
	m, env := newRemoteTestManager(t)
	env.listResults = []listResult{{out: "No Sockets found in /tmp.\n"}}
	id, _ := m.SpawnRemote("mac", "main", "", "", 80, 24)
	env.pty(0).Close()
	waitFor(t, "ended", func() bool { return sessionState(m, id) == stateEnded })
	if env.startCount() != 1 {
		t.Errorf("starts = %d, want no reattach", env.startCount())
	}
}

func TestDropRetriesWhileNetworkDown(t *testing.T) {
	m, env := newRemoteTestManager(t)
	env.listResults = []listResult{
		{err: errors.New("connect timed out")},
		{err: errors.New("connect timed out")},
		{out: screenLs("main")},
	}
	id, _ := m.SpawnRemote("mac", "main", "", "", 80, 24)
	env.pty(0).Close()
	waitFor(t, "running after retries", func() bool {
		return env.startCount() == 2 && sessionState(m, id) == stateRunning
	})
	env.mu.Lock()
	sleeps := append([]time.Duration(nil), env.sleeps...)
	env.mu.Unlock()
	if len(sleeps) < 3 || sleeps[0] != time.Second || sleeps[1] != 2*time.Second || sleeps[2] != 4*time.Second {
		t.Errorf("sleeps = %v, want 1s,2s,4s", sleeps)
	}
}

func TestDisconnectStopsWithoutReconnect(t *testing.T) {
	m, env := newRemoteTestManager(t)
	env.listResults = []listResult{{out: screenLs("main")}}
	id, _ := m.SpawnRemote("mac", "main", "", "", 80, 24)
	m.Disconnect(id)
	waitFor(t, "disconnected", func() bool { return sessionState(m, id) == stateDisconnected })
	time.Sleep(50 * time.Millisecond)
	if env.startCount() != 1 {
		t.Errorf("starts = %d, want 1 (no reconnect)", env.startCount())
	}

	m.Reconnect(id)
	waitFor(t, "reattached", func() bool { return env.startCount() == 2 && sessionState(m, id) == stateRunning })
}

func TestKillOnRemoteDisconnects(t *testing.T) {
	m, env := newRemoteTestManager(t)
	id, _ := m.SpawnRemote("mac", "main", "", "", 80, 24)
	m.Kill(id)
	waitFor(t, "disconnected", func() bool { return sessionState(m, id) == stateDisconnected })
	for _, call := range env.sshCalls {
		if strings.HasPrefix(call[len(call)-1], "cs -k") {
			t.Error("kill must not end the Mac session")
		}
	}
}

func TestEndRunsCsKill(t *testing.T) {
	m, env := newRemoteTestManager(t)
	env.listResults = []listResult{{out: screenLs("main")}}
	id, _ := m.SpawnRemote("mac", "main", "", "", 80, 24)
	m.End(id)
	waitFor(t, "ended", func() bool { return sessionState(m, id) == stateEnded })
	env.mu.Lock()
	defer env.mu.Unlock()
	found := false
	for _, call := range env.sshCalls {
		if call[len(call)-1] == "cs -k main" {
			found = true
		}
	}
	if !found {
		t.Errorf("ssh calls = %v, want cs -k main", env.sshCalls)
	}
}

func TestEndFailureKeepsSessionRunning(t *testing.T) {
	var mu sync.Mutex
	var broadcasts []string
	m, env := newRemoteTestManager(t)
	m.broadcast = func(b []byte) {
		mu.Lock()
		broadcasts = append(broadcasts, string(b))
		mu.Unlock()
	}
	env.endErr = errors.New("permission denied")
	env.listResults = []listResult{{out: screenLs("main")}}
	id, _ := m.SpawnRemote("mac", "main", "", "", 80, 24)
	m.End(id)
	waitFor(t, "error broadcast", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range broadcasts {
			if strings.Contains(b, `"type":"error"`) && strings.Contains(b, "permission denied") {
				return true
			}
		}
		return false
	})
	if got := sessionState(m, id); got != stateRunning {
		t.Errorf("state = %s, want running", got)
	}
}

func TestRemoveStopsReconnect(t *testing.T) {
	m, env := newRemoteTestManager(t)
	env.listResults = []listResult{{out: screenLs("main")}}
	id, _ := m.SpawnRemote("mac", "main", "", "", 80, 24)
	m.Remove(id)
	time.Sleep(50 * time.Millisecond)
	if env.startCount() != 1 {
		t.Errorf("starts = %d, want 1", env.startCount())
	}
	if sessionState(m, id) != "missing" {
		t.Error("session should be gone")
	}
}

func TestDiscoverAdoptsNewSessionsOnly(t *testing.T) {
	m, env := newRemoteTestManager(t)
	if _, err := m.SpawnRemote("mac", "main", "", "", 80, 24); err != nil {
		t.Fatal(err)
	}
	env.listResults = []listResult{{out: screenLs("main", "agent")}}
	m.Discover("mac")
	list := m.List()
	if len(list) != 2 {
		t.Fatalf("sessions = %+v, want main + agent", list)
	}
	if list[1].Name != "agent" || list[1].Host != "mac" {
		t.Errorf("adopted = %+v", list[1])
	}
	if !strings.HasSuffix(env.started[1], `"cs agent"`) {
		t.Errorf("adopt cmdline = %s", env.started[1])
	}
}

func TestDiscoverBroadcastsErrorWhenUnreachable(t *testing.T) {
	var mu sync.Mutex
	var got []string
	m, env := newRemoteTestManager(t)
	m.broadcast = func(b []byte) {
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
	}
	env.listResults = []listResult{{err: errors.New("Permission denied (publickey)")}}
	m.Discover("mac")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], `"host":"mac"`) || !strings.Contains(got[0], "publickey") {
		t.Errorf("broadcasts = %v", got)
	}
}

func TestBrowseRemote(t *testing.T) {
	m, env := newRemoteTestManager(t)
	env.browseOut = "/Users/me/Desktop\nMy Projects\n"
	res, err := m.BrowseRemote("mac", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "/Users/me/Desktop" || res.Parent != "/Users/me" || res.Sep != "/" {
		t.Errorf("result = %+v", res)
	}
	if len(res.Entries) != 1 || res.Entries[0].Name != "My Projects" || !res.Entries[0].IsDir {
		t.Errorf("entries = %+v", res.Entries)
	}
	last := env.sshCalls[len(env.sshCalls)-1]
	if !strings.HasPrefix(last[len(last)-1], "cd -- ~ ") {
		t.Errorf("empty path should use host default ~, got %s", last[len(last)-1])
	}
	if _, err := m.BrowseRemote("nope", ""); err == nil {
		t.Error("expected unknown host error")
	}
}

// collectErrors records error broadcasts from m and returns a snapshot function.
func collectErrors(m *Manager) func() []string {
	var mu sync.Mutex
	var errs []string
	m.broadcast = func(b []byte) {
		if strings.Contains(string(b), `"type":"error"`) {
			mu.Lock()
			errs = append(errs, string(b))
			mu.Unlock()
		}
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), errs...)
	}
}

func TestEndFailureAfterDropRevertsToDisconnected(t *testing.T) {
	m, env := newRemoteTestManager(t)
	errs := collectErrors(m)
	gate := make(chan struct{})
	env.endGate = gate
	env.listResults = []listResult{{out: screenLs("main")}}
	id, err := m.SpawnRemote("mac", "main", "", "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	m.End(id)
	env.pty(0).Close()
	waitFor(t, "ended after drop", func() bool { return sessionState(m, id) == stateEnded })

	env.mu.Lock()
	env.endErr = errors.New("permission denied")
	env.endGate = nil
	env.mu.Unlock()
	close(gate)
	waitFor(t, "disconnected after failed end", func() bool {
		return sessionState(m, id) == stateDisconnected
	})
	if got := errs(); len(got) != 1 || !strings.Contains(got[0], "permission denied") {
		t.Errorf("errors = %v, want one end failure", got)
	}

	m.Reconnect(id)
	waitFor(t, "reattached", func() bool {
		return env.startCount() == 2 && sessionState(m, id) == stateRunning
	})
}

func TestSpawnRemoteSentinelErrors(t *testing.T) {
	m, _ := newRemoteTestManager(t)
	m.maxSessions = 1
	if _, err := m.SpawnRemote("mac", "a", "", "", 80, 24); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SpawnRemote("mac", "a", "", "", 80, 24); !errors.Is(err, errMaxSessions) {
		t.Errorf("err = %v, want errMaxSessions", err)
	}
	m.maxSessions = 8
	_, err := m.SpawnRemote("mac", "a", "", "", 80, 24)
	if !errors.Is(err, errSessionOpen) || !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("err = %v, want errSessionOpen naming a", err)
	}
}

func TestDiscoverContinuesPastAdoptFailure(t *testing.T) {
	m, env := newRemoteTestManager(t)
	errs := collectErrors(m)
	env.startErrFor = `"cs a"`
	env.listResults = []listResult{{out: screenLs("a", "b", "c")}}
	m.Discover("mac")
	var names []string
	for _, s := range m.List() {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "b,c" {
		t.Errorf("adopted = %v, want b,c", names)
	}
	if got := errs(); len(got) != 1 || !strings.Contains(got[0], "start failed") {
		t.Errorf("errors = %v, want one adopt failure", got)
	}
}

func TestDiscoverStopsAtMaxSessions(t *testing.T) {
	m, env := newRemoteTestManager(t)
	errs := collectErrors(m)
	m.maxSessions = 2
	if _, err := m.SpawnRemote("mac", "x", "", "", 80, 24); err != nil {
		t.Fatal(err)
	}
	env.listResults = []listResult{{out: screenLs("x", "a", "b", "c")}}
	m.Discover("mac")
	if got := len(m.List()); got != 2 {
		t.Errorf("sessions = %d, want 2", got)
	}
	got := errs()
	if len(got) != 1 || !strings.Contains(got[0], "max sessions") {
		t.Errorf("errors = %v, want one max-sessions error", got)
	}
}

func TestStaleAttachAfterDisconnectIsDiscarded(t *testing.T) {
	m, env := newRemoteTestManager(t)
	gate := make(chan struct{})
	blocked := make(chan struct{})
	env.startGate = gate
	env.startBlocked = blocked
	env.listResults = []listResult{{out: screenLs("main")}}
	id, err := m.SpawnRemote("mac", "main", "", "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	env.pty(0).Close()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocked reattach")
	}
	m.Disconnect(id)
	close(gate)
	waitFor(t, "second start", func() bool { return env.startCount() == 2 })
	stale := env.pty(1)
	select {
	case <-stale.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("stale pty was not closed")
	}
	time.Sleep(50 * time.Millisecond)
	if got := sessionState(m, id); got != stateDisconnected {
		t.Errorf("state = %s, want disconnected", got)
	}
	if got := env.startCount(); got != 2 {
		t.Errorf("starts = %d, want 2", got)
	}
}

func TestStaleLoopDoesNotAdvanceBackoff(t *testing.T) {
	m, env := newRemoteTestManager(t)
	gate := make(chan struct{})
	blocked := make(chan struct{})
	env.listGate = gate
	env.listBlocked = blocked
	env.listResults = []listResult{{err: errors.New("connect timed out")}}
	id, err := m.SpawnRemote("mac", "main", "", "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	env.pty(0).Close()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocked list")
	}
	m.Disconnect(id)
	close(gate)
	time.Sleep(50 * time.Millisecond)
	env.mu.Lock()
	sleeps := append([]time.Duration(nil), env.sleeps...)
	env.mu.Unlock()
	if len(sleeps) != 1 {
		t.Errorf("sleeps = %v, want only the first 1s (stale loop must not advance)", sleeps)
	}
	if got := sessionState(m, id); got != stateDisconnected {
		t.Errorf("state = %s, want disconnected", got)
	}
}

// stuckPty ignores Close so a test can observe state before readLoop ends.
type stuckPty struct{ *fakePty }

func (stuckPty) Close() error { return nil }

func TestLocalKillMarksExited(t *testing.T) {
	m, _ := newRemoteTestManager(t)
	inner := newFakePty()
	t.Cleanup(func() { inner.Close() })
	m.startPty = func(string, string, uint16, uint16) (ptyProc, error) {
		return stuckPty{inner}, nil
	}
	id, err := m.Spawn("cmd", "", "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	m.Kill(id)
	for _, s := range m.List() {
		if s.ID == id && (s.Running || s.State != stateExited) {
			t.Errorf("after kill: running=%v state=%s, want false/exited", s.Running, s.State)
		}
	}
}

// csKillCalls returns the `cs -k` commands sent over ssh so far.
func csKillCalls(env *fakeEnv) []string {
	env.mu.Lock()
	defer env.mu.Unlock()
	var calls []string
	for _, call := range env.sshCalls {
		if strings.HasPrefix(call[len(call)-1], "cs -k") {
			calls = append(calls, call[len(call)-1])
		}
	}
	return calls
}

func TestEndSkipsCsKillWhenSessionNotListed(t *testing.T) {
	m, env := newRemoteTestManager(t)
	id, err := m.SpawnRemote("mac", "main", "", "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	// screen matches by prefix: cs -k main would kill cs-main2.
	env.listResults = []listResult{{out: screenLs("main2")}}
	m.End(id)
	waitFor(t, "ended", func() bool { return sessionState(m, id) == stateEnded })
	if got := csKillCalls(env); len(got) != 0 {
		t.Errorf("cs -k calls = %v, want none", got)
	}
}

func TestEndListFailureKeepsSessionAndSkipsCsKill(t *testing.T) {
	m, env := newRemoteTestManager(t)
	errs := collectErrors(m)
	id, err := m.SpawnRemote("mac", "main", "", "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	env.listResults = []listResult{{err: errors.New("connect timed out")}}
	m.End(id)
	waitFor(t, "error broadcast", func() bool { return len(errs()) == 1 })
	if !strings.Contains(errs()[0], "connect timed out") {
		t.Errorf("errors = %v", errs())
	}
	if got := csKillCalls(env); len(got) != 0 {
		t.Errorf("cs -k calls = %v, want none", got)
	}
	if got := sessionState(m, id); got != stateRunning {
		t.Errorf("state = %s, want running", got)
	}
	s := m.get(id)
	s.mu.Lock()
	endRequested := s.endRequested
	s.mu.Unlock()
	if endRequested {
		t.Error("endRequested should be cleared after a failed end")
	}
}

func TestEndWhileReconnectingDoesNotReattach(t *testing.T) {
	m, env := newRemoteTestManager(t)
	listGate := make(chan struct{})
	listBlocked := make(chan struct{})
	endGate := make(chan struct{})
	env.listResults = []listResult{{out: screenLs("main")}}
	id, err := m.SpawnRemote("mac", "main", "", "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	env.mu.Lock()
	env.listGate, env.listBlocked, env.endGate = listGate, listBlocked, endGate
	env.mu.Unlock()
	env.pty(0).Close()
	select {
	case <-listBlocked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reconnect list")
	}
	m.End(id)
	close(listGate)
	time.Sleep(50 * time.Millisecond)
	if got := env.startCount(); got != 1 {
		t.Errorf("starts = %d, want 1 (no reattach after end)", got)
	}
	close(endGate)
	waitFor(t, "ended", func() bool { return sessionState(m, id) == stateEnded })
	time.Sleep(50 * time.Millisecond)
	if got := env.startCount(); got != 1 {
		t.Errorf("starts = %d, want 1", got)
	}
}

func TestEndFailureWhileReconnectingShowsDisconnected(t *testing.T) {
	m, env := newRemoteTestManager(t)
	listGate := make(chan struct{})
	listBlocked := make(chan struct{})
	env.listResults = []listResult{{out: screenLs("main")}}
	id, err := m.SpawnRemote("mac", "main", "", "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	env.mu.Lock()
	env.listGate, env.listBlocked = listGate, listBlocked
	env.endErr = errors.New("permission denied")
	env.mu.Unlock()
	env.pty(0).Close()
	select {
	case <-listBlocked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reconnect list")
	}
	m.End(id)
	close(listGate)
	waitFor(t, "disconnected", func() bool { return sessionState(m, id) == stateDisconnected })
}

func TestDiscoverStoresAndClearsHostError(t *testing.T) {
	var mu sync.Mutex
	var got []string
	m, env := newRemoteTestManager(t)
	m.broadcast = func(b []byte) {
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
	}
	env.listResults = []listResult{
		{err: errors.New("Permission denied (publickey)")},
		{out: "No Sockets found in /tmp.\n"},
	}
	m.Discover("mac")
	if errs := m.HostErrors(); !strings.Contains(errs["mac"], "publickey") {
		t.Fatalf("HostErrors = %v, want mac publickey error", errs)
	}
	m.Discover("mac")
	if errs := m.HostErrors(); len(errs) != 0 {
		t.Errorf("HostErrors = %v, want empty after success", errs)
	}
	mu.Lock()
	defer mu.Unlock()
	last := got[len(got)-1]
	if !strings.Contains(last, `"type":"host_ok"`) || !strings.Contains(last, `"host":"mac"`) {
		t.Errorf("broadcasts = %v, want host_ok for mac last", got)
	}
}

func TestHostErrorsReturnsCopy(t *testing.T) {
	m, env := newRemoteTestManager(t)
	env.listResults = []listResult{{err: errors.New("down")}}
	m.Discover("mac")
	errs := m.HostErrors()
	errs["mac"] = "changed"
	delete(errs, "mac")
	if got := m.HostErrors(); !strings.Contains(got["mac"], "down") {
		t.Errorf("HostErrors = %v, internal map was modified", got)
	}
}

func TestSpawnRemoteEmptyCwdUsesHostDefault(t *testing.T) {
	m, env := newRemoteTestManager(t)
	m.SetHosts([]HostConfig{{ID: "mac", Label: "Mac", SSH: "mac", DefaultCwd: "~/Projects"}})
	if _, err := m.SpawnRemote("mac", "api", "", "", 80, 24); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.started[0], `cs api ~/'Projects'`) {
		t.Errorf("cmdline = %s, want host default folder", env.started[0])
	}
}

func TestListIncludesRemoteName(t *testing.T) {
	m, _ := newRemoteTestManager(t)
	if _, err := m.SpawnRemote("mac", "API work", "", "", 80, 24); err != nil {
		t.Fatal(err)
	}
	list := m.List()
	if len(list) != 1 || list[0].Name != "API work" || list[0].RemoteName != "API-work" {
		t.Errorf("list = %+v, want name 'API work', remote name API-work", list)
	}
}
