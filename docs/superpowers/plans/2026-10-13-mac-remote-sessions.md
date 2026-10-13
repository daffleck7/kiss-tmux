# Mac Remote Sessions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let KISS-TMUX on Windows run and monitor Claude sessions on a headless Mac (via `ssh mac cs …`) next to local sessions, with per-machine tabs and colors, auto-discovery, and auto-reconnect.

**Architecture:** A remote session is a normal ConPTY session whose command line is `ssh.exe -t … mac "cs <name> <cwd>"`; `screen` on the Mac (via the existing `cs` script) provides persistence. One-off SSH calls (`screen -ls`, `cs -k`, folder listing) run with `BatchMode=yes` through an injectable runner. The Manager gains injectable PTY/SSH/sleep functions so the remote state machine is unit-testable with fakes. The frontend adds a machine tab bar and a `--host-color` CSS variable.

**Tech Stack:** Go 1.23 (`UserExistsError/conpty`, `gorilla/websocket`), vanilla JS + xterm.js, Windows OpenSSH client.

**Spec:** `docs/superpowers/specs/2026-10-13-mac-remote-sessions-design.md`

**Ground rules for every task:**
- Work in `C:\Users\you\kiss-tmux`. Run tests with `go test ./...`.
- Never delete, move or rename `kiss-tmux.ico`, `kiss-tmux.png`, or `C:\Users\you\Desktop\KISS-TMUX.lnk`. They are untracked; do not run `git clean`.
- Do not `git add` untracked files you didn't create (`config.json`, `session_*.txt`, `dump_sessions.go`, `kiss-tmux.exe~`, `.claude/`).
- Commit messages end with:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  ```

## File Structure

| File | Status | Responsibility |
|------|--------|----------------|
| `config.go` | Create | `AppConfig`, `HostConfig`, `parseConfig`, `loadConfig` (moved out of `server.go`) |
| `config_test.go` | Create | Config defaults and host validation |
| `remote.go` | Create | Pure helpers: quoting, name validation, ssh arg builders, `screen -ls`/browse parsing, `Backoff`, reconnect decisions, `runSSHCommand` |
| `remote_test.go` | Create | Tests for all pure helpers |
| `pty.go` | Create | `ptyProc` interface, `startConPty` with idempotent Close + exit watcher |
| `session.go` | Modify | Session fields (Host, RemoteName, State…), injectable deps, remote spawn, reconnect loop, Disconnect/Reconnect/End, Discover, BrowseRemote |
| `remote_manager_test.go` | Create | Manager remote behavior with fake PTY and fake SSH |
| `protocol.go` | Modify | `Host` on ClientMsg/ServerMsg, `Host`/`State` on SessionInfo, browse types |
| `protocol_test.go` | Modify | New field round-trips |
| `server.go` | Modify | New WS message types, `?host=` browse, config passed in |
| `main.go` | Modify | Load config, set hosts, startup discovery |
| `web/index.html` | Modify | Tab bar, banner, dialog ids, context menu items |
| `web/style.css` | Modify | Layout for tab bar, host colors, state badges |
| `web/app.js` | Modify | Hosts, tabs, filtering, dialog/browse per host, new actions |
| `README.md`, `CHANGELOG.md` | Modify | Document remote sessions |

---

### Task 1: Config with hosts

**Files:**
- Create: `config.go`
- Create: `config_test.go`
- Modify: `server.go` (remove `AppConfig` and `loadConfig`, lines ~196-221)

- [ ] **Step 1: Write the failing tests**

Create `config_test.go`:

```go
package main

import "testing"

func TestParseConfigDefaults(t *testing.T) {
	cfg, warnings, err := parseConfig(nil, `C:\Users\me`)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if cfg.DefaultCwd != `C:\Users\me` {
		t.Errorf("DefaultCwd = %q", cfg.DefaultCwd)
	}
	if cfg.LocalLabel != "Windows" || cfg.LocalColor != "#3b82f6" {
		t.Errorf("local defaults = %q %q", cfg.LocalLabel, cfg.LocalColor)
	}
	if cfg.Hosts == nil || len(cfg.Hosts) != 0 {
		t.Errorf("Hosts = %#v, want empty non-nil slice", cfg.Hosts)
	}
}

func TestParseConfigHosts(t *testing.T) {
	raw := `{"default_cwd":"C:\\Desktop","hosts":[
		{"id":"mac","label":"Mac","ssh":"mac","default_cwd":"~/Desktop/My Projects","color":"#f97316"},
		{"id":"box"}
	]}`
	cfg, warnings, err := parseConfig([]byte(raw), `C:\Users\me`)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(cfg.Hosts) != 2 {
		t.Fatalf("hosts = %d, want 2", len(cfg.Hosts))
	}
	mac := cfg.Hosts[0]
	if mac.ID != "mac" || mac.SSH != "mac" || mac.DefaultCwd != "~/Desktop/My Projects" {
		t.Errorf("mac = %+v", mac)
	}
	box := cfg.Hosts[1]
	if box.SSH != "box" || box.Label != "box" || box.Color != "#f97316" || box.DefaultCwd != "~" {
		t.Errorf("box defaults = %+v", box)
	}
}

func TestParseConfigRejectsBadHosts(t *testing.T) {
	raw := `{"hosts":[{"id":"Bad ID"},{"id":"mac"},{"id":"mac"}]}`
	cfg, warnings, err := parseConfig([]byte(raw), "home")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hosts) != 1 || cfg.Hosts[0].ID != "mac" {
		t.Errorf("hosts = %+v, want only mac", cfg.Hosts)
	}
	if len(warnings) != 2 {
		t.Errorf("warnings = %v, want 2", warnings)
	}
}

func TestParseConfigInvalidJSON(t *testing.T) {
	if _, _, err := parseConfig([]byte("{nope"), "home"); err == nil {
		t.Error("expected error for invalid JSON")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestParseConfig ./...`
Expected: FAIL — `undefined: parseConfig`

- [ ] **Step 3: Implement `config.go` and remove the old config code from `server.go`**

Create `config.go`:

```go
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
)

const (
	defaultLocalLabel = "Windows"
	defaultLocalColor = "#3b82f6"
	defaultHostColor  = "#f97316"
)

var hostIDPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

// HostConfig describes a remote machine reachable over SSH.
type HostConfig struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	SSH        string `json:"ssh"`
	DefaultCwd string `json:"default_cwd"`
	Color      string `json:"color"`
}

// AppConfig holds user-configurable settings loaded from config.json.
type AppConfig struct {
	DefaultCwd string       `json:"default_cwd"`
	LocalLabel string       `json:"local_label"`
	LocalColor string       `json:"local_color"`
	Hosts      []HostConfig `json:"hosts"`
}

// parseConfig decodes config JSON and applies defaults. Invalid or duplicate
// hosts are dropped and described in the returned warnings.
func parseConfig(data []byte, home string) (AppConfig, []string, error) {
	var cfg AppConfig
	if len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return AppConfig{}, nil, fmt.Errorf("parse config.json: %w", err)
		}
	}
	if cfg.DefaultCwd == "" {
		cfg.DefaultCwd = home
	}
	if cfg.LocalLabel == "" {
		cfg.LocalLabel = defaultLocalLabel
	}
	if cfg.LocalColor == "" {
		cfg.LocalColor = defaultLocalColor
	}

	var warnings []string
	seen := map[string]bool{}
	hosts := make([]HostConfig, 0, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		if !hostIDPattern.MatchString(h.ID) {
			warnings = append(warnings, fmt.Sprintf("host %q skipped: id must match [a-z0-9_-]+", h.ID))
			continue
		}
		if seen[h.ID] {
			warnings = append(warnings, fmt.Sprintf("host %q skipped: duplicate id", h.ID))
			continue
		}
		seen[h.ID] = true
		if h.SSH == "" {
			h.SSH = h.ID
		}
		if h.Label == "" {
			h.Label = h.ID
		}
		if h.Color == "" {
			h.Color = defaultHostColor
		}
		if h.DefaultCwd == "" {
			h.DefaultCwd = "~"
		}
		hosts = append(hosts, h)
	}
	cfg.Hosts = hosts
	return cfg, warnings, nil
}

// loadConfig reads config.json next to the executable, falling back to defaults.
func loadConfig() AppConfig {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}

	var data []byte
	if exePath, err := os.Executable(); err == nil {
		configPath := filepath.Join(filepath.Dir(exePath), "config.json")
		raw, err := os.ReadFile(configPath)
		switch {
		case err == nil:
			data = raw
		case !errors.Is(err, os.ErrNotExist):
			log.Printf("config: read %s: %v", configPath, err)
		}
	}

	cfg, warnings, err := parseConfig(data, home)
	if err != nil {
		log.Printf("config: %v (using defaults)", err)
		cfg, _, _ = parseConfig(nil, home)
	}
	for _, w := range warnings {
		log.Printf("config: %s", w)
	}
	return cfg
}
```

In `server.go`, delete the `AppConfig` type and the whole `loadConfig` function (the block starting `// AppConfig holds user-configurable settings loaded from config.json.` through the end of `loadConfig`). Leave `cfg := loadConfig()` in `SetupHTTP` for now (Task 8 changes it).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go vet ./... && go test ./...`
Expected: PASS (vet may complain about unused imports in `server.go` — none should become unused, since `os`/`filepath` are still used by browse).

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go server.go
git commit -m "feat: add hosts to config and move config loading to config.go"
```

---

### Task 2: Pure remote helpers

**Files:**
- Create: `remote.go`
- Create: `remote_test.go`

- [ ] **Step 1: Write the failing tests**

Create `remote_test.go`:

```go
package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":           `'plain'`,
		"with space":      `'with space'`,
		"it's":            `'it'\''s'`,
		"":                `''`,
		"$(rm -rf ~)":     `'$(rm -rf ~)'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestQuoteRemotePath(t *testing.T) {
	cases := map[string]string{
		"":                              "~",
		"~":                             "~",
		"~/":                            "~/",
		"~/Desktop/My Projects": `~/'Desktop/My Projects'`,
		"/Users/me/My Repo":            `'/Users/me/My Repo'`,
		"~other":                        `'~other'`,
	}
	for in, want := range cases {
		if got := quoteRemotePath(in); got != want {
			t.Errorf("quoteRemotePath(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestValidRemoteName(t *testing.T) {
	for _, ok := range []string{"main", "my-app", "app_2", "A1"} {
		if !validRemoteName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "a b", "a;b", "x/y", "ü"} {
		if validRemoteName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestSlugRemoteName(t *testing.T) {
	cases := map[string]string{
		"My App!":     "My-App",
		"  agent 2  ": "agent-2",
		"a--b":        "a-b",
		"under_score": "under_score",
		"!!!":         "",
		"":            "",
	}
	for in, want := range cases {
		if got := slugRemoteName(in); got != want {
			t.Errorf("slugRemoteName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAttachArgs(t *testing.T) {
	got := attachArgs("mac", "myapp", "~/Desktop/My Projects")
	want := []string{
		"-t", "-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3",
		"mac", `cs myapp ~/'Desktop/My Projects'`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attachArgs = %q\nwant %q", got, want)
	}
	reattach := attachArgs("mac", "myapp", "")
	if reattach[len(reattach)-1] != "cs myapp" {
		t.Errorf("reattach remote command = %q", reattach[len(reattach)-1])
	}
}

func TestBatchArgs(t *testing.T) {
	if got := listArgs("mac"); got[len(got)-1] != "screen -ls" || got[0] != "-T" {
		t.Errorf("listArgs = %q", got)
	}
	if got := endArgs("mac", "myapp"); got[len(got)-1] != "cs -k myapp" {
		t.Errorf("endArgs = %q", got)
	}
	joined := strings.Join(listArgs("mac"), " ")
	if !strings.Contains(joined, "BatchMode=yes") || !strings.Contains(joined, "ConnectTimeout=5") {
		t.Errorf("listArgs missing batch options: %s", joined)
	}
}

func TestBrowseRemoteCommand(t *testing.T) {
	got := browseRemoteCommand("~/Desktop/My Projects")
	if !strings.HasPrefix(got, `cd -- ~/'Desktop/My Projects' && pwd && find . `) {
		t.Errorf("browseRemoteCommand = %s", got)
	}
}

func TestWindowsCommandLine(t *testing.T) {
	got := windowsCommandLine(`C:\Windows\System32\OpenSSH\ssh.exe`, []string{"-t", "mac", "cs main 'a b'"})
	want := `C:\Windows\System32\OpenSSH\ssh.exe -t mac "cs main 'a b'"`
	if got != want {
		t.Errorf("windowsCommandLine = %s\nwant %s", got, want)
	}
}

func TestParseScreenList(t *testing.T) {
	out := "There are screens on:\n" +
		"\t4242.cs-main\t(10/13/26 11:00:00)\t(Detached)\r\n" +
		"\t5151.cs-my-app\t(10/13/26 11:05:00)\t(Attached)\n" +
		"\t6161.unrelated\t(10/13/26 11:06:00)\t(Detached)\n" +
		"3 Sockets in /var/folders/xx/.screen.\n"
	got := parseScreenList(out)
	want := []string{"main", "my-app"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseScreenList = %q, want %q", got, want)
	}
	if got := parseScreenList("No Sockets found in /var/folders/xx/.screen.\n"); len(got) != 0 {
		t.Errorf("no sockets = %q, want empty", got)
	}
}

func TestParseBrowseOutput(t *testing.T) {
	path, dirs, err := parseBrowseOutput("/Users/me/Desktop\nMy Projects\nNotes\n")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/Users/me/Desktop" || !reflect.DeepEqual(dirs, []string{"My Projects", "Notes"}) {
		t.Errorf("got %q %q", path, dirs)
	}
	_, dirs, err = parseBrowseOutput("/empty\n")
	if err != nil || len(dirs) != 0 || dirs == nil {
		t.Errorf("empty dir: dirs=%#v err=%v", dirs, err)
	}
	if _, _, err := parseBrowseOutput("zsh: no such file\n"); err == nil {
		t.Error("expected error for non-path first line")
	}
}

func TestRemoteParent(t *testing.T) {
	if got := remoteParent("/Users/me"); got != "/Users" {
		t.Errorf("parent = %q", got)
	}
	if got := remoteParent("/"); got != "" {
		t.Errorf("root parent = %q, want empty", got)
	}
}

func TestBackoff(t *testing.T) {
	b := newBackoff()
	var got []time.Duration
	for i := 0; i < 8; i++ {
		got = append(got, b.Next())
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("backoff sequence = %v", got)
		}
	}
	b.Reset()
	if d := b.Next(); d != time.Second {
		t.Errorf("after reset = %v, want 1s", d)
	}
}

func TestNextReconnectAction(t *testing.T) {
	if got := nextReconnectAction(errors.New("timeout"), nil, "main"); got != actionRetry {
		t.Errorf("list error -> %v, want retry", got)
	}
	if got := nextReconnectAction(nil, []string{"x", "main"}, "main"); got != actionAttach {
		t.Errorf("present -> %v, want attach", got)
	}
	if got := nextReconnectAction(nil, []string{"x"}, "main"); got != actionEnded {
		t.Errorf("absent -> %v, want ended", got)
	}
}

func TestStateAfterExit(t *testing.T) {
	cases := []struct {
		remote, disconnect, end bool
		want                    string
	}{
		{false, false, false, stateExited},
		{true, false, false, stateReconnecting},
		{true, true, false, stateDisconnected},
		{true, false, true, stateEnded},
		{true, true, true, stateEnded},
	}
	for _, c := range cases {
		if got := stateAfterExit(c.remote, c.disconnect, c.end); got != c.want {
			t.Errorf("stateAfterExit(%v,%v,%v) = %s, want %s", c.remote, c.disconnect, c.end, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL — `undefined: shellQuote` (and others)

- [ ] **Step 3: Implement `remote.go`**

```go
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Session states. Local sessions only use running and exited.
const (
	stateRunning      = "running"
	stateReconnecting = "reconnecting"
	stateDisconnected = "disconnected"
	stateEnded        = "ended"
	stateExited       = "exited"
)

// remoteSessionPrefix is prepended to screen session names by the Mac's cs script.
const remoteSessionPrefix = "cs-"

// sshTimeout bounds one-off SSH commands (list, end, browse).
const sshTimeout = 10 * time.Second

// stableConnection is how long a connection must last before backoff resets.
const stableConnection = 60 * time.Second

// createNoWindow stops one-off ssh.exe calls from flashing a console window.
const createNoWindow = 0x08000000

var (
	remoteNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	screenLinePattern = regexp.MustCompile(`^\s+\d+\.(\S+)\s`)
)

// validRemoteName reports whether name is safe to pass to cs unquoted.
func validRemoteName(name string) bool {
	return remoteNamePattern.MatchString(name)
}

// slugRemoteName turns a display name into a cs-safe session name.
func slugRemoteName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			lastDash = false
		case !lastDash && b.Len() > 0:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// quoteRemotePath quotes a remote path, leaving a leading ~/ unquoted so it expands.
func quoteRemotePath(p string) string {
	switch {
	case p == "" || p == "~":
		return "~"
	case p == "~/":
		return "~/"
	case strings.HasPrefix(p, "~/"):
		return "~/" + shellQuote(p[2:])
	default:
		return shellQuote(p)
	}
}

// attachArgs builds ssh arguments that start or reattach a cs session.
func attachArgs(alias, name, cwd string) []string {
	remoteCmd := "cs " + name
	if cwd != "" {
		remoteCmd += " " + quoteRemotePath(cwd)
	}
	return []string{
		"-t", "-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3",
		alias, remoteCmd,
	}
}

// batchArgs builds ssh arguments for a non-interactive command that must never prompt.
func batchArgs(alias, remoteCmd string) []string {
	return []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", alias, remoteCmd}
}

// listArgs builds ssh arguments that list screen sessions.
func listArgs(alias string) []string {
	return batchArgs(alias, "screen -ls")
}

// endArgs builds ssh arguments that end a cs session.
func endArgs(alias, name string) []string {
	return batchArgs(alias, "cs -k "+name)
}

// browseRemoteCommand prints the resolved directory, then its non-hidden subdirectories.
// It avoids globs because the Mac's login shell is zsh, which errors on no-match globs.
func browseRemoteCommand(p string) string {
	return "cd -- " + quoteRemotePath(p) +
		" && pwd && find . -mindepth 1 -maxdepth 1 -type d ! -name '.*'" +
		" | sed 's|^\\./||' | LC_ALL=C sort -f"
}

// windowsCommandLine joins an executable and args into a CreateProcess command line.
func windowsCommandLine(exe string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, syscall.EscapeArg(exe))
	for _, arg := range args {
		parts = append(parts, syscall.EscapeArg(arg))
	}
	return strings.Join(parts, " ")
}

// sshExecutable prefers Windows' built-in OpenSSH, which behaves well under ConPTY.
func sshExecutable() string {
	builtin := filepath.Join(os.Getenv("SystemRoot"), "System32", "OpenSSH", "ssh.exe")
	if _, err := os.Stat(builtin); err == nil {
		return builtin
	}
	return "ssh"
}

// parseScreenList extracts cs session names (prefix stripped) from `screen -ls` output.
func parseScreenList(output string) []string {
	var names []string
	for _, line := range strings.Split(output, "\n") {
		match := screenLinePattern.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if match == nil || !strings.HasPrefix(match[1], remoteSessionPrefix) {
			continue
		}
		name := strings.TrimPrefix(match[1], remoteSessionPrefix)
		if validRemoteName(name) {
			names = append(names, name)
		}
	}
	return names
}

// parseBrowseOutput splits browseRemoteCommand output into the absolute path and dir names.
func parseBrowseOutput(output string) (string, []string, error) {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	if !strings.HasPrefix(lines[0], "/") {
		return "", nil, fmt.Errorf("unexpected browse output: %q", lines[0])
	}
	dirs := []string{}
	for _, line := range lines[1:] {
		if line != "" {
			dirs = append(dirs, line)
		}
	}
	return lines[0], dirs, nil
}

// remoteParent returns the POSIX parent of p, or "" at the root.
func remoteParent(p string) string {
	if p == "/" {
		return ""
	}
	return path.Dir(p)
}

// Backoff produces exponentially growing delays capped at Max.
type Backoff struct {
	Base    time.Duration
	Max     time.Duration
	attempt int
}

// newBackoff returns the reconnect backoff: 1s, 2s, 4s … capped at 30s.
func newBackoff() *Backoff {
	return &Backoff{Base: time.Second, Max: 30 * time.Second}
}

// Next returns the next delay and advances the sequence.
func (b *Backoff) Next() time.Duration {
	delay := b.Base << b.attempt
	if delay <= 0 || delay >= b.Max {
		return b.Max
	}
	b.attempt++
	return delay
}

// Reset starts the sequence over.
func (b *Backoff) Reset() {
	b.attempt = 0
}

type reconnectAction int

const (
	actionRetry reconnectAction = iota
	actionAttach
	actionEnded
)

// nextReconnectAction decides what to do after listing sessions during reconnect.
func nextReconnectAction(listErr error, names []string, name string) reconnectAction {
	if listErr != nil {
		return actionRetry
	}
	for _, n := range names {
		if n == name {
			return actionAttach
		}
	}
	return actionEnded
}

// stateAfterExit is the state a session enters when its PTY closes.
func stateAfterExit(remote, disconnectRequested, endRequested bool) string {
	switch {
	case !remote:
		return stateExited
	case endRequested:
		return stateEnded
	case disconnectRequested:
		return stateDisconnected
	default:
		return stateReconnecting
	}
}

// sshRunner runs ssh with args and returns stdout.
type sshRunner func(ctx context.Context, args []string) ([]byte, error)

// runSSHCommand runs ssh.exe without a console window; stderr is folded into the error.
func runSSHCommand(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, sshExecutable(), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// isRemoteExitError reports whether err is a non-zero exit of the remote command
// itself (as opposed to ssh failing to connect, which exits 255).
func isRemoteExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() != 255
}
```

Note on `screen -ls`: it exits non-zero even when it prints sessions, so callers must treat `isRemoteExitError` as success and parse the output.

Note on Backoff: `delay >= b.Max` returns Max without incrementing, which gives `…16, 30, 30` as tested and avoids shift overflow.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add remote.go remote_test.go
git commit -m "feat: add ssh command builders and screen/browse parsing"
```

---

### Task 3: Protocol fields

**Files:**
- Modify: `protocol.go`
- Modify: `protocol_test.go`

- [ ] **Step 1: Write the failing tests** (append to `protocol_test.go`)

```go
func TestClientMsgHost(t *testing.T) {
	raw := `{"type":"spawn","host":"mac","name":"agent","cwd":"~/x"}`
	var msg ClientMsg
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Host != "mac" {
		t.Errorf("host = %q, want mac", msg.Host)
	}
}

func TestSessionInfoHostAndState(t *testing.T) {
	data, err := json.Marshal(SessionInfo{ID: "a", Name: "n", Host: "mac", State: "reconnecting"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"host":"mac"`) || !strings.Contains(s, `"state":"reconnecting"`) {
		t.Errorf("json = %s", s)
	}
}

func TestServerMsgErrorHost(t *testing.T) {
	data, _ := json.Marshal(ServerMsg{Type: "error", Host: "mac", Error: "boom"})
	if !strings.Contains(string(data), `"host":"mac"`) {
		t.Errorf("json = %s", data)
	}
	data, _ = json.Marshal(ServerMsg{Type: "exited"})
	if strings.Contains(string(data), `"host"`) {
		t.Errorf("host should be omitted when empty: %s", data)
	}
}
```

Add `"strings"` to the imports of `protocol_test.go`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run "Host|State" ./...`
Expected: FAIL — `msg.Host undefined`

- [ ] **Step 3: Implement** — replace `protocol.go` with:

```go
package main

// ClientMsg is the envelope for all client→server WebSocket messages.
type ClientMsg struct {
	Type    string `json:"type"`
	Session string `json:"session,omitempty"`
	Host    string `json:"host,omitempty"`
	Cmd     string `json:"cmd,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Name    string `json:"name,omitempty"`
	Data    string `json:"data,omitempty"`
	Cols    uint16 `json:"cols,omitempty"`
	Rows    uint16 `json:"rows,omitempty"`
}

// ServerMsg is the envelope for all server→client WebSocket messages.
type ServerMsg struct {
	Type     string        `json:"type"`
	Session  string        `json:"session,omitempty"`
	Host     string        `json:"host,omitempty"`
	Data     string        `json:"data,omitempty"`
	Code     int           `json:"code,omitempty"`
	Error    string        `json:"error,omitempty"`
	Sessions []SessionInfo `json:"sessions,omitempty"`
}

// SessionInfo describes a session in the sessions list broadcast.
// Host is "" for local sessions.
type SessionInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Cmd     string `json:"cmd"`
	Running bool   `json:"running"`
	Host    string `json:"host"`
	State   string `json:"state"`
}

// DirEntry is one directory in a browse listing.
type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
}

// BrowseResult is the JSON shape returned by /api/browse.
type BrowseResult struct {
	Path    string     `json:"path"`
	Parent  string     `json:"parent"`
	Entries []DirEntry `json:"entries"`
	Drives  []string   `json:"drives"`
	Sep     string     `json:"sep"`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS. (`server.go`'s browse handler declares its own local `type DirEntry struct`; it shadows the new package-level type and still compiles. Task 8 removes it.)

- [ ] **Step 5: Commit**

```bash
git add protocol.go protocol_test.go
git commit -m "feat: add host and state fields to websocket protocol"
```

---

### Task 4: Injectable PTY and session state (no behavior change)

**Files:**
- Create: `pty.go`
- Modify: `session.go`

This task refactors so later tasks can test with a fake PTY. Existing tests in `session_test.go` must keep passing unchanged.

- [ ] **Step 1: Create `pty.go`**

```go
package main

import (
	"context"
	"sync"
	"time"

	"github.com/UserExistsError/conpty"
)

// ptyProc is the subset of a pseudo-terminal the Manager needs.
type ptyProc interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(width, height int) error
	Close() error
}

// ptyStarter starts cmdline in a new pseudo-terminal.
type ptyStarter func(cmdline, cwd string, cols, rows uint16) (ptyProc, error)

// exitDrainDelay lets the reader collect final output before the PTY closes.
const exitDrainDelay = 200 * time.Millisecond

// conPtyProc makes Close idempotent (ConPty.Close double-closes handles otherwise)
// and closes the PTY when the child exits, because ConPTY keeps the output pipe open
// after exit and Read would block forever.
type conPtyProc struct {
	*conpty.ConPty
	once     sync.Once
	cancel   context.CancelFunc
	closeErr error
}

// Close closes the pseudo-console once.
func (c *conPtyProc) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.closeErr = c.ConPty.Close()
	})
	return c.closeErr
}

// startConPty starts cmdline under ConPTY and watches for process exit.
func startConPty(cmdline, cwd string, cols, rows uint16) (ptyProc, error) {
	opts := []conpty.ConPtyOption{conpty.ConPtyDimensions(int(cols), int(rows))}
	if cwd != "" {
		opts = append(opts, conpty.ConPtyWorkDir(cwd))
	}
	cpty, err := conpty.Start(cmdline, opts...)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc := &conPtyProc{ConPty: cpty, cancel: cancel}
	go func() {
		if _, err := cpty.Wait(ctx); err == nil {
			time.Sleep(exitDrainDelay)
			proc.Close()
		}
	}()
	return proc, nil
}
```

- [ ] **Step 2: Update `session.go`**

Replace the import block, `Session`, `Manager`, `NewManager`, `sessionCount`, `Spawn`, `readLoop`, `Input`, `Resize`, `Kill`, `Remove`, and `List` with the versions below (keep `RingBuffer`, `genID`, `Rename`, `Replay`, `SessionsMessage`, `broadcastSessions` as they are).

```go
import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)
```

```go
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
}

// NewManager creates a Manager allowing maxSessions per machine.
func NewManager(maxSessions int, broadcast func([]byte)) *Manager {
	return &Manager{
		broadcast:   broadcast,
		maxSessions: maxSessions,
		hosts:       map[string]HostConfig{},
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
		backoff: newBackoff(),
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

// onPtyEnd moves a session to its post-exit state. Task 6 adds reconnect.
func (m *Manager) onPtyEnd(s *Session) {
	s.mu.Lock()
	s.Running = false
	if s.removed {
		s.mu.Unlock()
		return
	}
	s.State = stateAfterExit(s.Host != "", s.disconnectRequested, s.endRequested)
	s.mu.Unlock()

	if s.Host == "" {
		exitMsg := ServerMsg{Type: "exited", Session: s.ID, Code: 0}
		if encoded, err := json.Marshal(exitMsg); err == nil {
			m.broadcast(encoded)
		}
	}
	m.broadcastSessions()
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

// Kill stops a local session. Task 6 makes it disconnect remote sessions.
func (m *Manager) Kill(id string) {
	s := m.get(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	wasRunning := s.Running
	s.Running = false
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
```

In `List`, build the info with the new fields:

```go
		info := SessionInfo{
			ID:      s.ID,
			Name:    s.Name,
			Cmd:     s.Cmd,
			Running: s.Running,
			Host:    s.Host,
			State:   s.State,
		}
```

Also replace the `Rename` and `Replay` bodies' `v, ok := m.sessions.Load(id)` lookups with `m.get(id)` for consistency:

```go
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

// Replay returns the buffered output for a session.
func (m *Manager) Replay(id string) []byte {
	s := m.get(id)
	if s == nil {
		return nil
	}
	return s.buf.Bytes()
}
```

- [ ] **Step 3: Run all tests**

Run: `go vet ./... && go test ./...`
Expected: PASS (existing `TestManager*` tests spawn real `cmd.exe` through `startConPty`).

- [ ] **Step 4: Commit**

```bash
git add pty.go session.go
git commit -m "refactor: make PTY injectable and track session state"
```

---

### Task 5: Remote spawn

**Files:**
- Modify: `session.go` (append)
- Create: `remote_manager_test.go`

- [ ] **Step 1: Write the failing tests**

Create `remote_manager_test.go`:

```go
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
func (f *fakePty) Resize(int, int) error      { return nil }
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
	// listResults are consumed in order by `screen -ls` calls; the last one repeats.
	listResults []listResult
	endErr      error
	browseOut   string
	sleeps      []time.Duration
}

type listResult struct {
	out string
	err error
}

func (e *fakeEnv) startPty(cmdline, cwd string, cols, rows uint16) (ptyProc, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := newFakePty()
	e.started = append(e.started, cmdline)
	e.ptys = append(e.ptys, p)
	return p, nil
}

func (e *fakeEnv) runSSH(ctx context.Context, args []string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sshCalls = append(e.sshCalls, args)
	remote := args[len(args)-1]
	switch {
	case remote == "screen -ls":
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
	id, err := m.SpawnRemote("mac", "My App", "~/Desktop/My Projects", 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if env.startCount() != 1 {
		t.Fatalf("starts = %d, want 1", env.startCount())
	}
	cmdline := env.started[0]
	if !strings.Contains(cmdline, `cs My-App ~/'Desktop/My Projects'`) {
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
	id, err := m.SpawnRemote("mac", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.started[0], "cs s"+id) {
		t.Errorf("cmdline = %s, want generated name s%s", env.started[0], id)
	}
}

func TestSpawnRemoteRejectsUnknownHostAndDuplicates(t *testing.T) {
	m, _ := newRemoteTestManager(t)
	if _, err := m.SpawnRemote("nope", "a", "", 80, 24); err == nil {
		t.Error("expected unknown host error")
	}
	if _, err := m.SpawnRemote("mac", "a", "", 80, 24); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SpawnRemote("mac", "a", "", 80, 24); err == nil {
		t.Error("expected duplicate session error")
	}
}

func TestSessionCapIsPerMachine(t *testing.T) {
	m, _ := newRemoteTestManager(t)
	m.maxSessions = 1
	if _, err := m.SpawnRemote("mac", "a", "", 80, 24); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SpawnRemote("mac", "b", "", 80, 24); err == nil {
		t.Error("expected max sessions error on mac")
	}
	if got := m.hostSessionCount(""); got != 0 {
		t.Errorf("local count = %d, want 0", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run "SpawnRemote|SessionCap" ./...`
Expected: FAIL — `m.SpawnRemote undefined`

- [ ] **Step 3: Implement** — append to `session.go` and add `"fmt"`, `"log"`, `"strings"` to its imports:

```go
// SpawnRemote opens a new cs session called name (slugified) on hostID in cwd.
func (m *Manager) SpawnRemote(hostID, name, cwd string, cols, rows uint16) (string, error) {
	host, ok := m.hosts[hostID]
	if !ok {
		return "", fmt.Errorf("unknown host %q", hostID)
	}
	displayName := strings.TrimSpace(name)
	return m.spawnRemoteSession(host, slugRemoteName(name), displayName, cwd, cols, rows)
}

// spawnRemoteSession attaches to (or creates) remoteName on host and registers it.
// An empty remoteName gets a generated one.
func (m *Manager) spawnRemoteSession(host HostConfig, remoteName, displayName, cwd string, cols, rows uint16) (string, error) {
	m.remoteMu.Lock()
	defer m.remoteMu.Unlock()

	if m.hostSessionCount(host.ID) >= m.maxSessions {
		return "", errors.New("max sessions reached")
	}
	id := genID()
	if remoteName == "" {
		remoteName = "s" + id
	}
	if !validRemoteName(remoteName) {
		return "", fmt.Errorf("invalid session name %q (use letters, numbers, - or _)", remoteName)
	}
	if m.findRemote(host.ID, remoteName) != nil {
		return "", fmt.Errorf("session %q is already open for %s", remoteName, host.Label)
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
	if err := m.attach(s, host, cwd); err != nil {
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
func (m *Manager) attach(s *Session, host HostConfig, cwd string) error {
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
	if s.removed {
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add session.go remote_manager_test.go
git commit -m "feat: spawn remote sessions over ssh with per-machine cap"
```

---

### Task 6: Reconnect, Disconnect, Reconnect, End

**Files:**
- Modify: `session.go`
- Modify: `remote_manager_test.go` (append)

- [ ] **Step 1: Write the failing tests** (append to `remote_manager_test.go`)

```go
func TestDropReconnectsWhenSessionStillExists(t *testing.T) {
	m, env := newRemoteTestManager(t)
	env.listResults = []listResult{{out: screenLs("main")}}
	id, err := m.SpawnRemote("mac", "main", "", 80, 24)
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
	id, _ := m.SpawnRemote("mac", "main", "", 80, 24)
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
	id, _ := m.SpawnRemote("mac", "main", "", 80, 24)
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
	id, _ := m.SpawnRemote("mac", "main", "", 80, 24)
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
	id, _ := m.SpawnRemote("mac", "main", "", 80, 24)
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
	id, _ := m.SpawnRemote("mac", "main", "", 80, 24)
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
	id, _ := m.SpawnRemote("mac", "main", "", 80, 24)
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
	id, _ := m.SpawnRemote("mac", "main", "", 80, 24)
	m.Remove(id)
	time.Sleep(50 * time.Millisecond)
	if env.startCount() != 1 {
		t.Errorf("starts = %d, want 1", env.startCount())
	}
	if sessionState(m, id) != "missing" {
		t.Error("session should be gone")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run "Drop|Disconnect|KillOnRemote|End|RemoveStops" ./...`
Expected: FAIL — `m.Disconnect undefined` (and others)

- [ ] **Step 3: Implement**

Replace `onPtyEnd` in `session.go` with:

```go
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
```

Replace `Kill` with:

```go
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
	p := s.cpty
	s.mu.Unlock()

	if wasRunning {
		p.Close()
	}
}
```

Append to `session.go`:

```go
// listRemote returns cs session names on host. A non-zero exit from screen itself
// is normal (screen -ls exits 1 with or without sessions); only ssh failures are errors.
func (m *Manager) listRemote(host HostConfig) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sshTimeout)
	defer cancel()
	out, err := m.runSSH(ctx, listArgs(host.SSH))
	if err != nil && !isRemoteExitError(err) {
		return nil, fmt.Errorf("list sessions on %s: %w", host.Label, err)
	}
	return parseScreenList(string(out)), nil
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
		if err := m.attach(s, host, ""); err != nil {
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
	return s.removed || s.reconnectGen != gen || s.State != stateReconnecting
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
		ctx, cancel := context.WithTimeout(context.Background(), sshTimeout)
		defer cancel()
		if _, err := m.runSSH(ctx, endArgs(host.SSH, s.RemoteName)); err != nil {
			log.Printf("end %s on %s: %v", s.RemoteName, host.ID, err)
			s.mu.Lock()
			s.endRequested = false
			s.mu.Unlock()
			m.broadcastError(host.ID, s.ID, fmt.Sprintf("Could not end %s on %s: %v", s.RemoteName, host.Label, err))
			return
		}
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
	}()
}
```

Add `"context"` to the `session.go` imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./...` (if `-race` is unavailable because cgo is off, run `go test ./...`)
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add session.go remote_manager_test.go
git commit -m "feat: auto-reconnect remote sessions; add disconnect and end"
```

---

### Task 7: Discovery and remote browse

**Files:**
- Modify: `session.go` (append)
- Modify: `remote_manager_test.go` (append)

- [ ] **Step 1: Write the failing tests** (append)

```go
func TestDiscoverAdoptsNewSessionsOnly(t *testing.T) {
	m, env := newRemoteTestManager(t)
	if _, err := m.SpawnRemote("mac", "main", "", 80, 24); err != nil {
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run "Discover|BrowseRemote" ./...`
Expected: FAIL — `m.Discover undefined`

- [ ] **Step 3: Implement** (append to `session.go`)

```go
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
		m.broadcastError(host.ID, "", fmt.Sprintf("Can't reach %s: %v", host.Label, err))
		return
	}
	for _, name := range names {
		if m.findRemote(host.ID, name) != nil {
			continue
		}
		if _, err := m.spawnRemoteSession(host, name, name, "", 80, 24); err != nil {
			log.Printf("discover %s: adopt %s: %v", host.ID, name, err)
			m.broadcastError(host.ID, "", fmt.Sprintf("Couldn't open %s on %s: %v", name, host.Label, err))
			return
		}
	}
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add session.go remote_manager_test.go
git commit -m "feat: discover running Mac sessions and browse remote folders"
```

---

### Task 8: Server and main wiring

**Files:**
- Modify: `server.go`
- Modify: `main.go`

- [ ] **Step 1: Update the WebSocket switch in `readPump`** — replace the `case "spawn":` block and add new cases after `case "remove":`:

```go
		case "spawn":
			var id string
			var err error
			if msg.Host != "" {
				id, err = mgr.SpawnRemote(msg.Host, msg.Name, msg.Cwd, msg.Cols, msg.Rows)
			} else {
				id, err = mgr.Spawn(msg.Cmd, msg.Cwd, msg.Name, msg.Cols, msg.Rows)
			}
			if err != nil {
				log.Printf("spawn (host=%q name=%q): %v", msg.Host, msg.Name, err)
				errMsg := ServerMsg{Type: "error", Host: msg.Host, Error: err.Error()}
				if data, err := json.Marshal(errMsg); err == nil {
					c.safeSend(data)
				}
			} else {
				spawnedMsg := ServerMsg{Type: "spawned", Session: id}
				if data, err := json.Marshal(spawnedMsg); err == nil {
					c.safeSend(data)
				}
			}
```

```go
		case "disconnect":
			mgr.Disconnect(msg.Session)
		case "reconnect":
			mgr.Reconnect(msg.Session)
		case "end":
			mgr.End(msg.Session)
		case "discover":
			go mgr.Discover(msg.Host)
```

- [ ] **Step 2: Change `SetupHTTP` to take the config and support `?host=`**

Change the signature to `func SetupHTTP(hub *Hub, mgr *Manager, cfg AppConfig) http.Handler`, delete the line `cfg := loadConfig()`, and replace the whole `/api/browse` handler with:

```go
	// Directory listing API for the browse dialog. ?host=<id> lists a remote machine.
	mux.HandleFunc("/api/browse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		reqPath := r.URL.Query().Get("path")

		var result BrowseResult
		var err error
		if hostID := r.URL.Query().Get("host"); hostID != "" {
			result, err = mgr.BrowseRemote(hostID, reqPath)
		} else {
			result, err = browseLocal(reqPath)
		}
		if err != nil {
			log.Printf("browse: %v", err)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(result)
	})
```

Add below `SetupHTTP`:

```go
// browseLocal lists subdirectories of reqPath on this machine (home when empty).
func browseLocal(reqPath string) (BrowseResult, error) {
	if reqPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		reqPath = home
	}

	absPath, err := filepath.Abs(reqPath)
	if err != nil {
		return BrowseResult{}, err
	}
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return BrowseResult{}, err
	}

	dirs := []DirEntry{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || !e.IsDir() {
			continue
		}
		dirs = append(dirs, DirEntry{Name: e.Name(), IsDir: true})
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})

	// Drive letters on Windows for root navigation.
	drives := []string{}
	volume := filepath.VolumeName(absPath)
	if runtime.GOOS == "windows" && (absPath == volume+string(filepath.Separator) || absPath == volume) {
		for c := 'A'; c <= 'Z'; c++ {
			drive := string(c) + ":\\"
			if _, err := os.Stat(drive); err == nil {
				drives = append(drives, drive)
			}
		}
	}

	parent := filepath.Dir(absPath)
	if parent == absPath {
		parent = ""
	}
	return BrowseResult{Path: absPath, Parent: parent, Entries: dirs, Drives: drives, Sep: `\`}, nil
}
```

- [ ] **Step 3: Update `main.go`** — replace the body of `main` between `flag.Parse()` and `handler := …` and the handler line:

```go
	cfg := loadConfig()

	hub := NewHub()
	go hub.Run()

	mgr := NewManager(8, func(data []byte) {
		hub.broadcast <- data
	})
	mgr.SetHosts(cfg.Hosts)
	go mgr.DiscoverAll()

	handler := SetupHTTP(hub, mgr, cfg)
```

- [ ] **Step 4: Build and run all tests**

Run: `go vet ./... && go test ./... && go build -o kiss-tmux-dev.exe`
Expected: PASS and a `kiss-tmux-dev.exe` (ignored by `.gitignore`'s `kiss-tmux-*.exe`). Delete `kiss-tmux-dev.exe` afterwards.

- [ ] **Step 5: Commit**

```bash
git add server.go main.go
git commit -m "feat: wire remote session messages, browse and startup discovery"
```

---

### Task 9: Frontend markup and styles

**Files:**
- Modify: `web/index.html`
- Modify: `web/style.css`

- [ ] **Step 1: `web/index.html`** — make these edits:

Immediately after `<body>` add:

```html
  <!-- Machine tabs -->
  <div id="host-tabs" class="hidden"></div>
  <div id="host-banner" class="hidden"></div>
```

Change `<h3>New Session</h3>` to `<h3 id="dialog-title">New Session</h3>`.

Change the command label line to:

```html
      <label id="dialog-cmd-row">Command <input type="text" id="dialog-cmd" value="claude --dangerously-skip-permissions"></label>
```

Replace the context menu block with:

```html
  <!-- Context Menu -->
  <div id="context-menu" class="hidden">
    <div class="menu-item" data-action="rename">Rename</div>
    <div class="menu-item local-only" data-action="kill">Kill</div>
    <div class="menu-item remote-only" data-action="reconnect">Reconnect</div>
    <div class="menu-item remote-only" data-action="disconnect">Disconnect</div>
    <div class="menu-item remote-only danger" data-action="end">End on remote…</div>
    <div class="menu-item" data-action="remove">Remove</div>
  </div>
```

- [ ] **Step 2: `web/style.css`** — make these edits:

In `:root`, add `--host-color: #3b82f6;` and `--yellow: #f5c542;`.

Replace the `body` rule with:

```css
body {
  background: var(--bg);
  color: var(--text);
  font-family: 'Segoe UI', system-ui, sans-serif;
  height: 100vh;
  overflow: hidden;
  display: flex;
  flex-direction: column;
}
```

Change `#grid-view { height: 100vh; … }` to `flex: 1; min-height: 0;` (keep `padding: 8px;`), and `#focus-view { … height: 100vh; }` to `flex: 1; min-height: 0;`.

Change `.grid-cell { border: 1px solid var(--border); … }` to add a host-colored top edge:

```css
.grid-cell {
  background: var(--surface);
  border: 1px solid var(--border);
  border-top: 3px solid var(--host-color);
  border-radius: 6px;
  display: flex;
  flex-direction: column;
  overflow: hidden;
  cursor: pointer;
}

.grid-cell:hover {
  border-color: var(--host-color);
}
```

Change `.grid-cell-new:hover { border-color: var(--accent); … }` to `border-color: var(--host-color);`.

Change `#sidebar { … border-right: 1px solid var(--border); … }` to `border-right: 3px solid var(--host-color);`.

Change `.sidebar-tab.active { background: var(--accent); … }` to `background: var(--host-color);`.

Change `#focus-terminal { flex: 1; overflow: hidden; }` to add `border-top: 3px solid var(--host-color);`.

Change `.dialog-buttons` create button and inputs focus to host color: in `#new-session-dialog input:focus` and `#dialog-create` replace `var(--accent)` with `var(--host-color)` (leave other uses of `--accent` alone).

Append at the end of the file:

```css
/* Machine tabs */
#host-tabs {
  display: flex;
  gap: 4px;
  padding: 6px 8px 0;
  background: var(--surface);
  border-bottom: 1px solid var(--border);
  flex-shrink: 0;
}

.host-tab {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 14px;
  font-size: 12px;
  font-weight: bold;
  letter-spacing: 0.5px;
  color: var(--text-dim);
  background: transparent;
  border: none;
  border-bottom: 3px solid transparent;
  border-radius: 4px 4px 0 0;
  cursor: pointer;
}

.host-tab:hover {
  color: var(--text);
  background: rgba(255,255,255,0.04);
}

.host-tab.active {
  color: var(--text);
  border-bottom-color: var(--tab-color);
  background: rgba(255,255,255,0.06);
}

.host-swatch {
  width: 10px;
  height: 10px;
  border-radius: 2px;
  background: var(--tab-color);
}

/* Host error banner */
#host-banner {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 6px 12px;
  font-size: 12px;
  background: rgba(233, 69, 96, 0.15);
  border-bottom: 1px solid var(--accent);
  flex-shrink: 0;
}

#host-banner span {
  flex: 1;
}

#host-banner button {
  background: transparent;
  border: none;
  color: var(--text);
  font-size: 16px;
  cursor: pointer;
}

/* Remote session state */
.dot.reconnecting { background: var(--yellow); }

.state-badge {
  font-size: 11px;
  color: var(--text-dim);
  font-style: italic;
}

.reconnect-btn {
  font-size: 11px;
  padding: 1px 8px;
  border-radius: 3px;
  border: 1px solid var(--host-color);
  background: transparent;
  color: var(--text);
  cursor: pointer;
}

.menu-item.danger {
  color: var(--accent);
}
```

Also check `.tab-dot` uses the `.dot.*` color classes: the sidebar dot has class `tab-dot running|stopped`. Add after `.dot.stopped`:

```css
.tab-dot.running { background: var(--green); }
.tab-dot.stopped { background: var(--grey); }
.tab-dot.reconnecting { background: var(--yellow); }
```

(If `.tab-dot.running`/`.tab-dot.stopped` rules already exist elsewhere in the file, only add the `reconnecting` line.)

- [ ] **Step 3: Commit**

```bash
git add web/index.html web/style.css
git commit -m "feat: add machine tab bar, host colors and state styles"
```

---

### Task 10: Frontend logic

**Files:**
- Modify: `web/app.js`

The frontend has no automated test harness; verification is manual in Task 11.

- [ ] **Step 1: State and config** — replace the block from `// Default working directory…` through the end of the `fetch('/api/config')…` call with:

```js
// Default working directory for new local sessions (loaded from server config)
var DEFAULT_CWD = '';

// Machines: id '' is this Windows PC; others come from config.json "hosts".
var HOSTS = [{ id: '', label: 'Windows', color: '#3b82f6', default_cwd: '' }];
var activeHost = loadActiveHost();
var hostErrors = {};

function loadActiveHost() {
  try { return localStorage.getItem('kiss-tmux.activeHost') || ''; } catch (e) { return ''; }
}

function saveActiveHost() {
  try { localStorage.setItem('kiss-tmux.activeHost', activeHost); } catch (e) {}
}

function loadConfig() {
  fetch('/api/config')
    .then(function(r) { return r.json(); })
    .then(function(cfg) {
      if (cfg.default_cwd) DEFAULT_CWD = cfg.default_cwd;
      HOSTS = [{ id: '', label: cfg.local_label || 'Windows', color: cfg.local_color || '#3b82f6', default_cwd: DEFAULT_CWD }];
      (cfg.hosts || []).forEach(function(h) {
        HOSTS.push({ id: h.id, label: h.label, color: h.color, default_cwd: h.default_cwd });
      });
      if (!HOSTS.some(function(h) { return h.id === activeHost; })) activeHost = '';
      render();
    })
    .catch(function(err) { console.error('config load failed:', err); });
}

function hostById(id) {
  return HOSTS.find(function(h) { return h.id === id; }) || HOSTS[0];
}

function sessionHost(s) { return s.host || ''; }
function isRemote(s) { return !!s.host; }
function sessionState(s) { return s.state || (s.running ? 'running' : 'exited'); }

function visibleSessions() {
  return sessions.filter(function(s) { return sessionHost(s) === activeHost; });
}

function findSession(id) {
  return sessions.find(function(s) { return s.id === id; });
}

function dotClass(s) {
  var st = sessionState(s);
  if (st === 'running') return 'running';
  if (st === 'reconnecting') return 'reconnecting';
  return 'stopped';
}
```

- [ ] **Step 2: Errors** — replace `handleError` with:

```js
function handleError(msg) {
  console.error('server error:', msg.error, msg.session || '', msg.host || '');
  // Typing into a dead session produces a stream of these; the dot already shows it.
  if (msg.session && /not running/.test(msg.error)) return;
  var hostId = msg.host || '';
  if (!msg.host && msg.session) {
    var s = findSession(msg.session);
    if (s) hostId = sessionHost(s);
  }
  hostErrors[hostId] = msg.error;
  render();
}
```

- [ ] **Step 3: Rendering** — replace `render()` with:

```js
function render() {
  document.documentElement.style.setProperty('--host-color', hostById(activeHost).color);
  renderTabs();
  renderBanner();
  if (mode === 'grid') {
    renderGrid();
  } else {
    renderFocus();
  }
}

function renderTabs() {
  var bar = document.getElementById('host-tabs');
  bar.innerHTML = '';
  if (HOSTS.length < 2) {
    bar.classList.add('hidden');
    return;
  }
  bar.classList.remove('hidden');
  HOSTS.forEach(function(h) {
    var count = sessions.filter(function(s) { return sessionHost(s) === h.id; }).length;
    var tab = document.createElement('button');
    tab.className = 'host-tab' + (h.id === activeHost ? ' active' : '');
    tab.style.setProperty('--tab-color', h.color);
    var swatch = document.createElement('span');
    swatch.className = 'host-swatch';
    tab.appendChild(swatch);
    tab.appendChild(document.createTextNode(h.label.toUpperCase() + ' (' + count + ')'));
    tab.addEventListener('click', function() { switchHost(h.id); });
    bar.appendChild(tab);
  });
}

function renderBanner() {
  var banner = document.getElementById('host-banner');
  var text = hostErrors[activeHost];
  banner.innerHTML = '';
  if (!text) {
    banner.classList.add('hidden');
    return;
  }
  banner.classList.remove('hidden');
  var span = document.createElement('span');
  span.textContent = text;
  banner.appendChild(span);
  var close = document.createElement('button');
  close.textContent = '\u00d7';
  close.title = 'Dismiss';
  close.addEventListener('click', function() {
    delete hostErrors[activeHost];
    render();
  });
  banner.appendChild(close);
}

function switchHost(id) {
  activeHost = id;
  saveActiveHost();
  if (id) send({ type: 'discover', host: id });
  var visible = visibleSessions();
  if (mode === 'focus' && !visible.some(function(s) { return s.id === activeSession; })) {
    if (visible.length > 0) {
      activeSession = visible[0].id;
    } else {
      mode = 'grid';
    }
  }
  render();
}
```

In `renderGrid`, replace

```js
  container.dataset.count = sessions.length + 1;

  sessions.forEach(function(s) {
```

with

```js
  var visible = visibleSessions();
  container.dataset.count = visible.length + 1;

  visible.forEach(function(s) {
```

and replace the dot + name + header-contextmenu part (from `var dot = document.createElement('div');` through `header.appendChild(name);`) with:

```js
    var dot = document.createElement('div');
    dot.className = 'dot ' + dotClass(s);
    header.appendChild(dot);

    var name = document.createElement('span');
    name.className = 'name';
    name.textContent = s.name;
    name.addEventListener('dblclick', function(e) {
      e.stopPropagation();
      startRename(s.id, name);
    });
    header.appendChild(name);

    var st = sessionState(s);
    if (isRemote(s) && st !== 'running') {
      var badge = document.createElement('span');
      badge.className = 'state-badge';
      badge.textContent = st === 'reconnecting' ? 'reconnecting\u2026' : st;
      header.appendChild(badge);
    }
    if (isRemote(s) && st === 'disconnected') {
      var reconnectBtn = document.createElement('button');
      reconnectBtn.className = 'reconnect-btn';
      reconnectBtn.textContent = 'Reconnect';
      reconnectBtn.addEventListener('click', function(e) {
        e.stopPropagation();
        send({ type: 'reconnect', session: s.id });
      });
      header.appendChild(reconnectBtn);
    }
```

In `renderFocus`, replace `sessions.forEach(function(s) {` (the session tabs loop) with `visibleSessions().forEach(function(s) {`, and replace

```js
    tabDot.className = 'tab-dot ' + (s.running ? 'running' : 'stopped');
```

with

```js
    tabDot.className = 'tab-dot ' + dotClass(s);
```

In `activeTerminal`, replace the second line so it falls back to the first *visible* session:

```js
function activeTerminal() {
  if (activeSession && terminals[activeSession]) return terminals[activeSession].term;
  var visible = visibleSessions();
  if (visible.length > 0 && terminals[visible[0].id]) return terminals[visible[0].id].term;
  return null;
}
```

- [ ] **Step 4: New session dialog** — replace `showNewSessionDialog` and `createSessionFromDialog` with:

```js
function showNewSessionDialog() {
  var host = hostById(activeHost);
  var remote = !!host.id;
  document.getElementById('dialog-overlay').classList.remove('hidden');
  document.getElementById('dialog-title').textContent = 'New Session on ' + host.label;
  document.getElementById('dialog-name').value = '';
  document.getElementById('dialog-name').placeholder = remote ? 'letters, numbers, - or _' : 'optional';
  document.getElementById('dialog-cwd').value = remote ? host.default_cwd : DEFAULT_CWD;
  document.getElementById('dialog-cmd').value = 'claude --dangerously-skip-permissions';
  document.getElementById('dialog-cmd-row').classList.toggle('hidden', remote);
  document.getElementById('dialog-name').focus();
}

function createSessionFromDialog() {
  var name = document.getElementById('dialog-name').value.trim();
  var cwd = document.getElementById('dialog-cwd').value.trim();
  var msg = { type: 'spawn', cwd: cwd, name: name };
  if (activeHost) {
    msg.host = activeHost;
  } else {
    msg.cmd = document.getElementById('dialog-cmd').value.trim() || 'claude --dangerously-skip-permissions';
  }
  send(msg);
  hideNewSessionDialog();
}
```

- [ ] **Step 5: Folder browser** — replace `fetchDirectory` with:

```js
function joinPath(base, name, sep) {
  return base.charAt(base.length - 1) === sep ? base + name : base + sep + name;
}

function fetchDirectory(path) {
  var params = [];
  if (path) params.push('path=' + encodeURIComponent(path));
  if (activeHost) params.push('host=' + encodeURIComponent(activeHost));
  var url = '/api/browse' + (params.length ? '?' + params.join('&') : '');

  var list = document.getElementById('dir-browser-list');
  list.innerHTML = '';
  var loading = document.createElement('div');
  loading.className = 'dir-entry';
  loading.style.color = 'var(--text-dim)';
  loading.textContent = 'Loading\u2026';
  list.appendChild(loading);

  fetch(url)
    .then(function(r) { return r.json(); })
    .then(function(data) {
      list.innerHTML = '';
      if (data.error) {
        console.error('browse error:', data.error);
        var errEl = document.createElement('div');
        errEl.className = 'dir-entry';
        errEl.style.color = 'var(--accent)';
        errEl.textContent = data.error;
        list.appendChild(errEl);
        return;
      }
      var sep = data.sep || '\\';
      dirBrowserPath = data.path;
      dirBrowserParent = data.parent;

      document.getElementById('dir-browser-path').textContent = data.path;

      // Show drive letters at root on Windows
      if (data.drives && data.drives.length > 0) {
        data.drives.forEach(function(drive) {
          var el = document.createElement('div');
          el.className = 'dir-entry drive';
          el.textContent = drive;
          el.addEventListener('click', function() {
            fetchDirectory(drive);
          });
          list.appendChild(el);
        });
      }

      var entries = data.entries || [];
      entries.forEach(function(entry) {
        var el = document.createElement('div');
        el.className = 'dir-entry';
        el.textContent = '\uD83D\uDCC1 ' + entry.name;
        el.addEventListener('click', function() {
          fetchDirectory(joinPath(dirBrowserPath, entry.name, sep));
        });
        list.appendChild(el);
      });

      if (entries.length === 0 && (!data.drives || data.drives.length === 0)) {
        var empty = document.createElement('div');
        empty.className = 'dir-entry';
        empty.style.color = 'var(--text-dim)';
        empty.textContent = '(no subdirectories)';
        list.appendChild(empty);
      }
    })
    .catch(function(err) {
      console.error('browse fetch error:', err);
      list.innerHTML = '';
    });
}
```

- [ ] **Step 6: Context menu** — replace `showContextMenu` with:

```js
function showContextMenu(e, sessionId) {
  contextMenuTarget = sessionId;
  var s = findSession(sessionId);
  var remote = !!(s && isRemote(s));
  var menu = document.getElementById('context-menu');
  menu.querySelectorAll('.local-only').forEach(function(el) { el.classList.toggle('hidden', remote); });
  menu.querySelectorAll('.remote-only').forEach(function(el) { el.classList.toggle('hidden', !remote); });
  if (remote) {
    var st = sessionState(s);
    menu.querySelector('[data-action="end"]').textContent = 'End on ' + hostById(s.host).label + '\u2026';
    menu.querySelector('[data-action="reconnect"]').classList.toggle('hidden', st !== 'disconnected');
    menu.querySelector('[data-action="disconnect"]').classList.toggle('hidden', st === 'disconnected' || st === 'ended');
  }
  menu.classList.remove('hidden');
  menu.style.left = e.clientX + 'px';
  menu.style.top = e.clientY + 'px';
}
```

In `init`, inside the context-menu item click handler, add after the `kill` branch:

```js
      if (action === 'disconnect' && contextMenuTarget) {
        send({ type: 'disconnect', session: contextMenuTarget });
      }
      if (action === 'reconnect' && contextMenuTarget) {
        send({ type: 'reconnect', session: contextMenuTarget });
      }
      if (action === 'end' && contextMenuTarget) {
        var target = findSession(contextMenuTarget);
        if (target && confirm('Stop "' + target.name + '" on ' + hostById(target.host).label +
            '? Claude will be shut down there, not just disconnected.')) {
          send({ type: 'end', session: contextMenuTarget });
        }
      }
```

and change the `remove` branch's replacement-session pick to use visible sessions:

```js
      if (action === 'remove' && contextMenuTarget) {
        send({ type: 'remove', session: contextMenuTarget });
        if (activeSession === contextMenuTarget) {
          var removedId = contextMenuTarget;
          var next = visibleSessions().find(function(s) { return s.id !== removedId; });
          activeSession = next ? next.id : null;
        }
      }
```

- [ ] **Step 7: Init** — in `init()`, add `loadConfig();` immediately before `connect();`.

- [ ] **Step 8: Syntax check**

Run: `node --check web/app.js` (if Node is installed; otherwise skip — Task 11 loads it in the browser).
Expected: no output.

- [ ] **Step 9: Commit**

```bash
git add web/app.js
git commit -m "feat: machine tabs, per-host sessions and remote actions in UI"
```

---

### Task 11: Config, build, deploy, verify

**Files:**
- Modify (untracked, local only): `config.json`
- Modify: `README.md`, `CHANGELOG.md`

- [ ] **Step 1: Update local `config.json`** (do not commit — it is untracked and machine-specific):

```json
{
  "default_cwd": "C:\\Users\\you\\Desktop",
  "hosts": [
    {
      "id": "mac",
      "label": "Mac",
      "ssh": "mac",
      "default_cwd": "~/Desktop/My Projects",
      "color": "#f97316"
    }
  ]
}
```

- [ ] **Step 2: Verify passwordless SSH works** (prerequisite for discovery)

Run: `C:\Windows\System32\OpenSSH\ssh.exe -T -o BatchMode=yes mac "screen -ls; command -v cs"`
Expected: screen output and `/Users/me/.local/bin/cs`. If `Permission denied (publickey…)`, stop: the key is not installed yet (see spec "One-time Windows setup").

- [ ] **Step 3: Rebuild in place, preserving the desktop shortcut**

1. Check nothing is running: `tasklist | findstr kiss-tmux`. If it is, ask the user to close it (it holds sessions; do not kill it without asking).
2. Back up: `copy kiss-tmux.exe kiss-tmux.exe~` (overwrites the previous backup).
3. Build: `go build -o kiss-tmux.exe`
4. Confirm `kiss-tmux.ico` still exists and the shortcut still targets the exe:
   `powershell -c "(New-Object -ComObject WScript.Shell).CreateShortcut('C:\Users\you\Desktop\KISS-TMUX.lnk').TargetPath"`
   Expected: `C:\Users\you\kiss-tmux\kiss-tmux.exe`

- [ ] **Step 4: Manual verification** (launch from the desktop shortcut)

- [ ] Tab bar shows `WINDOWS (n)` blue and `MAC (n)` orange; switching tabs changes grid borders/sidebar color.
- [ ] Existing Windows sessions behave exactly as before (spawn, type, kill, remove, browse C:\).
- [ ] Mac tab → + New Session: title says "on Mac", no Command field, folder starts at `~/Desktop/My Projects`; Browse lists Mac folders including ones with spaces.
- [ ] Create session `test1` → Claude starts on the Mac (first time: `/login`).
- [ ] Close KISS-TMUX (window + process), relaunch → `test1` reappears in the Mac tab automatically.
- [ ] Turn Wi-Fi off ~20s on Windows, then on → header shows "reconnecting…", then resumes with scrollback intact.
- [ ] Right-click → Disconnect → state "disconnected", Reconnect button appears; `ssh mac screen -ls` still lists `cs-test1`. Reconnect works.
- [ ] Right-click → End on Mac… → confirm → state "ended"; `screen -ls` no longer lists it.
- [ ] Stop sshd reachability (wrong IP in `~/.ssh/config` temporarily) → Mac tab shows a dismissible red banner.

- [ ] **Step 5: Docs** — add to `README.md` a section after "Features":

```markdown
## Remote machines (Mac over SSH)

KISS-TMUX can drive Claude sessions on another machine over SSH, shown in their own
color-coded tab. The remote needs a `cs` command (a `screen` wrapper: `cs <name> <dir>`
starts/reattaches, `cs -k <name>` ends) and key-based SSH login.

1. Add a host to `~/.ssh/config` (e.g. `Host mac`).
2. Add it to `config.json` next to the exe:

   ```json
   "hosts": [{ "id": "mac", "label": "Mac", "ssh": "mac",
               "default_cwd": "~/Desktop/My Projects", "color": "#f97316" }]
   ```

Remote sessions are found automatically on startup and when you open the tab, and
reconnect on their own if the connection drops. Right-click a remote session for
**Disconnect** (Claude keeps running) or **End on Mac** (stops it).
```

Add to the top of `CHANGELOG.md` (match its existing heading style):

```markdown
## Unreleased

- Remote machine tabs: run and monitor Claude sessions on a Mac over SSH (`cs` + `screen`)
- Per-machine color coding for tabs, grid borders and sidebar
- Auto-discovery of running remote sessions and auto-reconnect with backoff
- Disconnect vs. End on remote; remote folder browser
- Fix: local sessions now show as exited when their process ends
```

- [ ] **Step 6: Run tests and commit**

```bash
go test ./...
git add README.md CHANGELOG.md
git commit -m "docs: document remote machine sessions"
```
