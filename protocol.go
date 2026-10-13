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
// Host is "" for local sessions; RemoteName is the cs name for remote ones.
type SessionInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Cmd        string `json:"cmd"`
	Running    bool   `json:"running"`
	Host       string `json:"host"`
	State      string `json:"state"`
	RemoteName string `json:"remote_name,omitempty"`
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
