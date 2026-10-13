package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBrowseLocalListsVisibleDirsSorted checks filtering, ordering and metadata.
func TestBrowseLocalListsVisibleDirsSorted(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b", "A", ".hidden"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := browseLocal(dir)
	if err != nil {
		t.Fatalf("browseLocal: %v", err)
	}
	var names []string
	for _, entry := range result.Entries {
		names = append(names, entry.Name)
	}
	if len(names) != 2 || names[0] != "A" || names[1] != "b" {
		t.Fatalf("entries = %v, want [A b]", names)
	}
	if result.Sep != `\` {
		t.Errorf("Sep = %q, want backslash", result.Sep)
	}
	if result.Path != dir {
		t.Errorf("Path = %q, want %q", result.Path, dir)
	}
	if result.Parent != filepath.Dir(dir) {
		t.Errorf("Parent = %q, want %q", result.Parent, filepath.Dir(dir))
	}
}

// TestBrowseLocalEmptyDirEntriesNonNil ensures Entries marshals as [] not null.
func TestBrowseLocalEmptyDirEntriesNonNil(t *testing.T) {
	result, err := browseLocal(t.TempDir())
	if err != nil {
		t.Fatalf("browseLocal: %v", err)
	}
	if result.Entries == nil {
		t.Fatal("Entries is nil, want empty non-nil slice")
	}
}

// TestSendInitialStateIncludesHostErrors checks a late client sees a startup error.
func TestSendInitialStateIncludesHostErrors(t *testing.T) {
	m, env := newRemoteTestManager(t)
	env.listResults = []listResult{{err: errors.New("Permission denied (publickey)")}}
	m.Discover("mac")

	client := &Client{send: make(chan []byte, 16)}
	sendInitialState(client, m)
	close(client.send)
	found := false
	for msg := range client.send {
		text := string(msg)
		if strings.Contains(text, `"type":"error"`) && strings.Contains(text, `"host":"mac"`) &&
			strings.Contains(text, "publickey") {
			found = true
		}
	}
	if !found {
		t.Error("initial state should include the stored host error")
	}
}

func TestAllowedOrigin(t *testing.T) {
	cases := []struct {
		origin string
		want   bool
	}{
		{"http://127.0.0.1:7777", true},
		{"http://localhost:7777", true},
		{"", true},
		{"http://evil.com", false},
		{"http://127.0.0.1:9999", false},
		{"http://127.0.0.1.evil.com:7777", false},
		{"http://127.0.0.1", false},
		{"not a url", false},
	}
	for _, tc := range cases {
		if got := allowedOrigin(tc.origin, 7777); got != tc.want {
			t.Errorf("allowedOrigin(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

// TestWebSocketRejectsForeignOrigin checks /ws refuses a cross-site page.
func TestWebSocketRejectsForeignOrigin(t *testing.T) {
	mgr := NewManager(8, func([]byte) {})
	handler := SetupHTTP(NewHub(), mgr, AppConfig{}, 7777)
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Origin", "http://evil.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}
