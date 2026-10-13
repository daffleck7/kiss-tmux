package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClientMsgSpawn(t *testing.T) {
	raw := `{"type":"spawn","cmd":"claude","cwd":"C:\\Users","name":"main"}`
	var msg ClientMsg
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "spawn" {
		t.Errorf("type = %q, want spawn", msg.Type)
	}
	if msg.Cmd != "claude" {
		t.Errorf("cmd = %q, want claude", msg.Cmd)
	}
	if msg.Cwd != `C:\Users` {
		t.Errorf("cwd = %q, want C:\\Users", msg.Cwd)
	}
	if msg.Name != "main" {
		t.Errorf("name = %q, want main", msg.Name)
	}
}

func TestClientMsgInput(t *testing.T) {
	raw := `{"type":"input","session":"abc123","data":"ls\r\n"}`
	var msg ClientMsg
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "input" || msg.Session != "abc123" || msg.Data != "ls\r\n" {
		t.Errorf("unexpected: %+v", msg)
	}
}

func TestClientMsgResize(t *testing.T) {
	raw := `{"type":"resize","session":"abc123","cols":120,"rows":40}`
	var msg ClientMsg
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Cols != 120 || msg.Rows != 40 {
		t.Errorf("cols=%d rows=%d, want 120,40", msg.Cols, msg.Rows)
	}
}

func TestServerMsgOutput(t *testing.T) {
	msg := ServerMsg{Type: "output", Session: "abc123", Data: "hello\x1b[32mworld\x1b[0m"}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ServerMsg
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != "output" || decoded.Session != "abc123" {
		t.Errorf("unexpected: %+v", decoded)
	}
	if decoded.Data != msg.Data {
		t.Errorf("data mismatch: got %q, want %q", decoded.Data, msg.Data)
	}
}

func TestServerMsgSessions(t *testing.T) {
	msg := ServerMsg{
		Type: "sessions",
		Sessions: []SessionInfo{
			{ID: "a1", Name: "main", Cmd: "claude", Running: true},
			{ID: "b2", Name: "test", Cmd: "cmd.exe", Running: false},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ServerMsg
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Sessions) != 2 {
		t.Fatalf("sessions count = %d, want 2", len(decoded.Sessions))
	}
	if decoded.Sessions[0].Running != true || decoded.Sessions[1].Running != false {
		t.Errorf("running flags wrong: %+v", decoded.Sessions)
	}
}

func TestServerMsgOmitsEmpty(t *testing.T) {
	msg := ServerMsg{Type: "exited", Session: "abc123", Code: 0}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	json.Unmarshal(data, &raw)
	if _, ok := raw["data"]; ok {
		t.Error("data field should be omitted when empty")
	}
	if _, ok := raw["error"]; ok {
		t.Error("error field should be omitted when empty")
	}
}

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

func TestSessionInfoRemoteName(t *testing.T) {
	data, _ := json.Marshal(SessionInfo{ID: "a", Host: "mac", RemoteName: "API-work"})
	if !strings.Contains(string(data), `"remote_name":"API-work"`) {
		t.Errorf("json = %s", data)
	}
	data, _ = json.Marshal(SessionInfo{ID: "a"})
	if strings.Contains(string(data), `remote_name`) {
		t.Errorf("remote_name should be omitted when empty: %s", data)
	}
}
