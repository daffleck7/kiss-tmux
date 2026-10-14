package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":       `'plain'`,
		"with space":  `'with space'`,
		"it's":        `'it'\''s'`,
		"":            `''`,
		"$(rm -rf ~)": `'$(rm -rf ~)'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestQuoteRemotePath(t *testing.T) {
	cases := map[string]string{
		"":                      "~",
		"~":                     "~",
		"~/":                    "~/",
		"~/Desktop/My Projects": `~/'Desktop/My Projects'`,
		"/Users/me/My Repo":     `'/Users/me/My Repo'`,
		"~other":                `'~other'`,
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
	for _, bad := range []string{"", "a b", "a;b", "x/y", "ü", "-k", "-", strings.Repeat("a", 33)} {
		if validRemoteName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestSlugRemoteName(t *testing.T) {
	cases := map[string]string{
		"My App!":                      "My-App",
		"  agent 2  ":                  "agent-2",
		"a--b":                         "a-b",
		"under_score":                  "under_score",
		"!!!":                          "",
		"":                             "",
		strings.Repeat("a", 31) + " b": strings.Repeat("a", 31),
		strings.Repeat("a", 40):        strings.Repeat("a", 32),
	}
	for in, want := range cases {
		if got := slugRemoteName(in); got != want {
			t.Errorf("slugRemoteName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAttachArgs(t *testing.T) {
	got := attachArgs("mac", "myapp", "~/Desktop/My Projects", "")
	want := []string{
		"-t", "-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3",
		"mac", `cs myapp ~/'Desktop/My Projects'`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attachArgs = %q\nwant %q", got, want)
	}
	reattach := attachArgs("mac", "myapp", "", "")
	if reattach[len(reattach)-1] != "cs myapp" {
		t.Errorf("reattach remote command = %q", reattach[len(reattach)-1])
	}
	withCmd := attachArgs("mac", "myapp", "~/code", "claude --dangerously-skip-permissions")
	if got := withCmd[len(withCmd)-1]; got != `cs myapp ~/'code' 'claude --dangerously-skip-permissions'` {
		t.Errorf("command remote command = %q", got)
	}
	noCwd := attachArgs("mac", "myapp", "", "it's")
	if got := noCwd[len(noCwd)-1]; got != `cs myapp '.' 'it'\''s'` {
		t.Errorf("command without cwd = %q", got)
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
	if !strings.HasPrefix(got, `cd -- ~/'Desktop/My Projects' >/dev/null && pwd && find . `) {
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
		"\t4242.cs-old\t(Detached)\n" +
		"\t777.cs-zombie\t(Dead ???)\n" +
		"\t888.cs-bad.name\t(Detached)\n" +
		"\t999.cs--flag\t(Detached)\n" +
		"3 Sockets in /var/folders/xx/.screen.\n"
	got, skipped := parseScreenList(out)
	want := []string{"main", "my-app", "old"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseScreenList = %q, want %q", got, want)
	}
	if wantSkipped := []string{"bad.name", "-flag"}; !reflect.DeepEqual(skipped, wantSkipped) {
		t.Errorf("skipped = %q, want %q", skipped, wantSkipped)
	}
	got, skipped = parseScreenList("No Sockets found in /var/folders/xx/.screen.\n")
	if len(got) != 0 || len(skipped) != 0 {
		t.Errorf("no sockets = %q / %q, want empty", got, skipped)
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

func TestIsRemoteCommandExitCode(t *testing.T) {
	for _, code := range []int{0, 1, 2} {
		if !isRemoteCommandExitCode(code) {
			t.Errorf("code %d should be a remote command exit", code)
		}
	}
	for _, code := range []int{126, 127, 255} {
		if isRemoteCommandExitCode(code) {
			t.Errorf("code %d should be an error", code)
		}
	}
}

func TestTimeoutIsNotRemoteExit(t *testing.T) {
	if isRemoteExitError(fmt.Errorf("x: %w", context.DeadlineExceeded)) {
		t.Error("deadline error must not count as remote exit")
	}
}
