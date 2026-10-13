package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed web
var webFS embed.FS

// allowedOrigin reports whether a WebSocket Origin may control KISS-TMUX: no Origin
// (a non-browser client) or this server's own page on 127.0.0.1 or localhost.
// Anything else could be a web page trying to drive the terminals.
func allowedOrigin(origin string, port int) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == fmt.Sprintf("127.0.0.1:%d", port) || u.Host == fmt.Sprintf("localhost:%d", port)
}

// newUpgrader returns a WebSocket upgrader that only accepts same-origin pages.
func newUpgrader(port int) *websocket.Upgrader {
	return &websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if allowedOrigin(origin, port) {
				return true
			}
			log.Printf("ws: rejected origin %q", origin)
			return false
		},
	}
}

type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mu         sync.Mutex
}

type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
}

// safeSend sends data to the client's send channel without panicking
// if the channel has been closed by the hub.
func (c *Client) safeSend(data []byte) {
	defer func() { recover() }()
	select {
	case c.send <- data:
	default:
	}
}

func NewHub() *Hub {
	return &Hub{
		broadcast:  make(chan []byte, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[*Client]bool),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
		case message := <-h.broadcast:
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) readPump(hub *Hub, mgr *Manager) {
	defer func() {
		hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(512 * 1024)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		var msg ClientMsg
		if err := c.conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("ws error: %v", err)
			}
			break
		}

		switch msg.Type {
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
		case "input":
			if err := mgr.Input(msg.Session, msg.Data); err != nil {
				errMsg := ServerMsg{Type: "error", Session: msg.Session, Error: err.Error()}
				if data, err := json.Marshal(errMsg); err == nil {
					c.safeSend(data)
				}
			}
		case "resize":
			mgr.Resize(msg.Session, msg.Cols, msg.Rows)
		case "kill":
			mgr.Kill(msg.Session)
		case "rename":
			mgr.Rename(msg.Session, msg.Name)
		case "remove":
			mgr.Remove(msg.Session)
		case "disconnect":
			mgr.Disconnect(msg.Session)
		case "reconnect":
			mgr.Reconnect(msg.Session)
		case "end":
			mgr.End(msg.Session)
		case "discover":
			go mgr.Discover(msg.Host)
		case "list":
			sessMsg := mgr.SessionsMessage()
			if data, err := json.Marshal(sessMsg); err == nil {
				c.safeSend(data)
			}
		}
	}
}

func sendInitialState(client *Client, mgr *Manager) {
	sessMsg := mgr.SessionsMessage()
	if data, err := json.Marshal(sessMsg); err == nil {
		client.safeSend(data)
	}

	for _, s := range mgr.List() {
		replay := mgr.Replay(s.ID)
		if len(replay) > 0 {
			msg := ServerMsg{Type: "output", Session: s.ID, Data: string(replay)}
			if data, err := json.Marshal(msg); err == nil {
				client.safeSend(data)
			}
		}
	}

	// Errors such as a startup "can't reach" happened before this client connected.
	for host, text := range mgr.HostErrors() {
		msg := ServerMsg{Type: "error", Host: host, Error: text}
		if data, err := json.Marshal(msg); err == nil {
			client.safeSend(data)
		}
	}
}

// SetupHTTP builds the HTTP handler. port is the port the server listens on,
// used to accept WebSocket connections only from its own page.
func SetupHTTP(hub *Hub, mgr *Manager, cfg AppConfig, port int) http.Handler {
	mux := http.NewServeMux()
	upgrader := newUpgrader(port)

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("upgrade error: %v", err)
			return
		}

		client := &Client{
			hub:  hub,
			conn: conn,
			send: make(chan []byte, 256),
		}
		go client.writePump()

		hub.register <- client
		sendInitialState(client, mgr)

		client.readPump(hub, mgr)
	})

	// Config endpoint so the frontend can read user settings.
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)
	})

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

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	return mux
}

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
