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

// maxRemoteNameLen is the longest session name accepted.
const maxRemoteNameLen = 32

// sshTimeout bounds one-off SSH commands (list, end, browse).
const sshTimeout = 10 * time.Second

// stableConnection is how long a connection must last before backoff resets.
const stableConnection = 60 * time.Second

// createNoWindow stops one-off ssh.exe calls from flashing a console window.
const createNoWindow = 0x08000000

var (
	remoteNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,31}$`)
	screenLinePattern = regexp.MustCompile(`^\s+\d+\.(\S+)\s`)
)

// validRemoteName reports whether name is safe to pass to cs unquoted.
// It rejects a leading "-" (would be read as a flag) and names over 32 characters.
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
	slug := b.String()
	if len(slug) > maxRemoteNameLen {
		slug = slug[:maxRemoteNameLen]
	}
	return strings.TrimRight(slug, "-")
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
// name must satisfy validRemoteName.
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
// name must satisfy validRemoteName.
func endArgs(alias, name string) []string {
	return batchArgs(alias, "cs -k "+name)
}

// browseRemoteCommand prints the resolved directory, then its non-hidden subdirectories.
// It avoids globs because the Mac's login shell is zsh, which errors on no-match globs.
func browseRemoteCommand(p string) string {
	return "cd -- " + quoteRemotePath(p) +
		" >/dev/null && pwd && find . -mindepth 1 -maxdepth 1 -type d ! -name '.*'" +
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
// skipped holds cs-* names that failed validRemoteName and were left out.
func parseScreenList(output string) (names, skipped []string) {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "(Dead") {
			continue
		}
		match := screenLinePattern.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if match == nil || !strings.HasPrefix(match[1], remoteSessionPrefix) {
			continue
		}
		name := strings.TrimPrefix(match[1], remoteSessionPrefix)
		if validRemoteName(name) {
			names = append(names, name)
		} else {
			skipped = append(skipped, name)
		}
	}
	return names, skipped
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
// It is not safe for concurrent use; callers hold the session lock.
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
	if containsName(names, name) {
		return actionAttach
	}
	return actionEnded
}

// containsName reports whether names includes name exactly.
func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.Bytes(), fmt.Errorf("ssh %s: %w", strings.Join(args, " "), ctxErr)
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.Bytes(), fmt.Errorf("%w: %s", err, msg)
		}
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

// isRemoteCommandExitCode reports whether code is an ordinary exit of the remote
// command. 255 is an ssh failure; 126 and 127 mean not executable / not found.
func isRemoteCommandExitCode(code int) bool {
	return code != 255 && code != 126 && code != 127
}

// isRemoteExitError reports whether err is a non-zero exit of the remote command
// itself, as opposed to ssh failing to connect (255) or the command being missing
// or not executable (126, 127).
func isRemoteExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && isRemoteCommandExitCode(exitErr.ExitCode())
}
