# KISS-TMUX

**Keep It Simple and Stupid TMUX** — a web-based terminal multiplexer in Go for Windows. Run multiple Claude Code sessions (or any command) in a browser grid, monitor them simultaneously, and switch to full-screen focus for interactive work.

No install. No config. No database. Just a single `.exe` that serves a terminal grid at `http://127.0.0.1:7777`.

## Why

When working with multiple long-running agents (like several Claude Code sessions), you want to *see* them all at once without juggling terminal windows. KISS-TMUX gives you:

- **Grid mode** — watch all sessions at a glance
- **Focus mode** — full-size terminal with a sidebar to jump between sessions
- **One binary, no dependencies** — download and run

## Quick Start

Pick whichever is most convenient:

### Option A — Download the exe (no Go needed)

1. Grab `kiss-tmux-windows-amd64.exe` from the [latest release](https://github.com/CinderZhang/kiss-tmux/releases/latest)
2. Double-click it (or run it from a terminal)

### Option B — `go install` (if you have Go 1.21+)

```bash
go install github.com/CinderZhang/kiss-tmux@latest
```

This builds and installs `kiss-tmux.exe` to `$GOPATH/bin` (or `$HOME/go/bin`). Then just run `kiss-tmux` from anywhere.

### Then

Your browser opens to `http://127.0.0.1:7777`. Click `+ New Session` to spawn a terminal.

### CLI flags

```
kiss-tmux [flags]

  --port int    Port to listen on (default 7777)
  --open        Auto-open browser (default true)
```

## Features

- **Grid mode** — auto-arranging CSS grid, up to 8 concurrent sessions per machine
- **Focus mode** — thin sidebar with 6-char session abbreviations + full-size terminal
- **WebGL rendering** — GPU-accelerated text rendering via `xterm-addon-webgl`, matching VSCode's terminal smoothness
- **Sidebar buttons** — grid toggle (☰), new session (+), one tab per session
- **Hover tooltips** — full session names on hover
- **Copy/paste** — `Ctrl+V` / `Ctrl+Shift+V` / right-click paste; `Ctrl+Shift+C` copy
- **Context menu** — right-click a session header: **Rename / Kill / Remove** for local sessions,
  **Rename / Disconnect / End / Remove** for remote ones (plus **Reconnect** when disconnected)
- **Rename** — right-click a session (grid header or sidebar tab) and choose Rename
- **Live reconnect** — refresh the browser and sessions replay from a 64KB ring buffer
- **Multiple browser tabs** — all tabs stay in sync via WebSocket broadcast
- **Default command** — `claude --dangerously-skip-permissions` (customizable per-session)

### Keyboard shortcuts

| Keys | Action |
|------|--------|
| `Ctrl+V` / `Ctrl+Shift+V` | Paste clipboard into active terminal |
| `Ctrl+Shift+C` | Copy selection from active terminal |
| `Ctrl+C` | Send SIGINT to the running process (standard terminal) |
| `Escape` | Close the new-session dialog |

All other navigation uses buttons so that terminal programs keep their keyboard input. Click session tabs to switch sessions. Click ☰ to return to grid view. Click + to open the new-session dialog.

## Remote machines (Mac over SSH)

KISS-TMUX can drive Claude sessions on another machine over SSH, each machine in its own
color-coded tab (`[WINDOWS] [MAC]`). The color shows on the tab, grid borders, sidebar and
focused terminal, so you always know which machine you're typing into.

**Remote requirements:** key-based SSH login, GNU `screen`, and a `cs` command on the
remote's non-interactive PATH (e.g. set in `~/.zshenv`):

| Command | Effect |
|---------|--------|
| `cs <name> <dir>` | Start session `<name>` in `<dir>`, or reattach if it exists |
| `cs <name> <dir> <command>` | Same, but the new session runs `<command>` instead of `claude` |
| `cs <name>` | Reattach |
| `cs -k <name>` | End the session (it won't be resumed) |
| `cs --ls` | `screen -ls` output, plus a `<tab>0.cs-<name><tab>(Saved)` line for each session lost in a reboot that `cs <name>` can resume |

`cs` must name its screen sessions `cs-<name>`; discovery reads `cs --ls`. A reference
implementation is in [`scripts/cs`](./scripts/cs) (zsh; copy it to `~/.local/bin/cs`).
Use GNU screen 5 or later (e.g. `brew install screen`): screen 4 drops the SGR mouse mode
that Claude's fullscreen UI uses for scrolling.

**Setup:**

1. Add the host to `~/.ssh/config` and install your key on it:

   ```
   Host mac
       HostName 100.x.y.z        # Tailscale IP, or the LAN IP
       User me
       ServerAliveInterval 30
   ```

2. Add it to `config.json` next to the exe:

   ```json
   {
     "default_cwd": "C:\\Users\\you\\Desktop",
     "hosts": [{ "id": "mac", "label": "Mac", "ssh": "mac",
                 "default_cwd": "~/Desktop/Projects", "color": "#f97316" }]
   }
   ```

   Optional: `local_label` (default `Windows`) and `local_color` (default `#3b82f6`).

**Behavior:**

- Running remote sessions are found automatically at startup and whenever you open the tab.
- A dropped connection reconnects on its own (1s, 2s, 4s ... up to 30s). Claude keeps running
  on the remote the whole time, inside `screen`.
- Right-click a remote session: **Disconnect** closes the SSH link only; **End on Mac...**
  (with confirmation) runs `cs -k` and stops Claude. **Remove** hides the tile; the session
  reappears on the next discovery.
- The folder browser in the New Session dialog lists the remote machine's folders. An empty
  folder uses the host's `default_cwd`.
- The **Command** box works on remote machines too (default
  `claude --dangerously-skip-permissions`). It only applies when the session is created;
  reattaching to an existing session keeps whatever it is already running.
- In focus view, a disconnected remote session shows a **Reconnect** bar above the terminal.
- Up to 8 sessions per machine.
- Session names allow letters, digits, `-` and `_`, up to 32 characters (other characters
  become `-`). **End** only runs `cs -k` when that exact name is listed, so it can never stop
  a different session that shares a prefix.
- **Rename** is display-only: the `cs` name stays the same, and the new name is lost when
  KISS-TMUX restarts.
- Remote sessions survive KISS-TMUX restarts (they live in `screen`). If the remote machine
  reboots or loses power, `cs` remembers each session's folder, command and Claude
  conversation, and KISS-TMUX resumes them automatically once the machine is back.
  **End** forgets a session for good.
- Noticing a dropped link can take up to ~90s (SSH keepalive: 3 missed 30s probes).
- If Claude on the Mac asks you to `/login`, that's because the macOS Keychain can be locked
  over SSH; run `/login` in that session.
- `~/.screenrc` on the remote must not set `autodetach off`, or closing the SSH link kills
  the session.
- Only pages served by KISS-TMUX itself (same origin) can control it; other websites can't
  open its WebSocket.

### Restarting without losing Claude conversations

`scripts/restart-and-resume.ps1` restarts KISS-TMUX and reopens every Claude conversation
that was running in it, using `claude --resume <id>`. If `kiss-tmux-new.exe` exists next to
`kiss-tmux.exe`, it is installed first (the old exe is kept as `kiss-tmux.exe~`).

```powershell
go build -o kiss-tmux-new.exe .          # optional: stage a new build
powershell -ExecutionPolicy Bypass -File scripts\restart-and-resume.ps1 -DryRun   # preview
powershell -ExecutionPolicy Bypass -File scripts\restart-and-resume.ps1
```

Run it from a normal PowerShell window. The restart continues in its own window, so it
keeps going after KISS-TMUX closes. Remote sessions are rediscovered automatically.

## Requirements

- **Windows 10 build 1809 or later** (for ConPTY support)
- A command to run in each session (defaults to Claude Code)

Only Windows is supported in v1. Cross-platform is a non-goal — ConPTY keeps the code simple.

## Architecture

Seven Go files. Two Go dependencies. One HTML, one CSS, one JS. No npm, no build toolchain.
Remote sessions are just `ssh.exe -t <host> "cs <name>"` running inside ConPTY like any other
command.

```
kiss-tmux/
├── main.go              # CLI entry, flag parsing, wiring
├── server.go            # Hub, Client, WebSocket handler, HTTP embed
├── session.go           # RingBuffer, Session, Manager (local + remote lifecycle)
├── protocol.go          # JSON message types
├── config.go            # config.json loading and validation (hosts, colors)
├── remote.go            # SSH/cs command building, screen -ls parsing, backoff
├── pty.go               # ConPTY wrapper behind a small interface (fakeable in tests)
├── web/
│   ├── index.html       # Grid view, focus view, dialog, context menu
│   ├── app.js           # WebSocket client, xterm.js terminal management
│   ├── style.css        # Dark theme
│   └── vendor/          # xterm.js + fit addon (vendored, no npm)
├── go.mod
└── go.sum
```

### Data flow

```
Browser (xterm.js)
  ↕ WebSocket JSON
Server (Hub → Client read/write pumps)
  ↕ Hub.broadcast channel
Manager (Session goroutines)
  ↕ io.Reader/io.Writer
ConPTY (Windows pseudo-console)
  ↕ kernel32.dll CreatePseudoConsole
Child Process (claude, cmd.exe, ...)
```

### Dependencies

- [`github.com/UserExistsError/conpty`](https://github.com/UserExistsError/conpty) — Windows ConPTY wrapper
- [`github.com/gorilla/websocket`](https://github.com/gorilla/websocket) — WebSocket server

That's it. Two Go dependencies. Frontend uses vanilla JS with [xterm.js](https://xtermjs.org/) + `xterm-addon-fit` + `xterm-addon-webgl` vendored from jsDelivr.

## Build from source

```bash
git clone https://github.com/CinderZhang/kiss-tmux
cd kiss-tmux
go build -o kiss-tmux.exe .
```

For a stripped release build:

```bash
go build -ldflags="-s -w" -o kiss-tmux.exe .
```

All web assets are embedded via `go:embed` — the binary is fully self-contained.

## Testing

```bash
go test -v ./...
```

80 tests covering the protocol (JSON roundtrip), ring buffer (circular overflow), config
validation, the WebSocket origin check, the session manager (ConPTY lifecycle with real
`cmd.exe` spawns), and the remote logic (spawn, reconnect, disconnect, end, discovery),
which runs against fake SSH and PTY implementations, so no Mac is needed.

## Non-goals

- Cross-platform support (Windows only)
- Authentication / multi-user (localhost only, bound to 127.0.0.1)
- Session persistence across server restarts (local sessions; remote sessions survive
  restarts because they live in `screen` on the remote)
- Task management, kanban, or agent-specific parsing
- React, build toolchains, or npm

## Releases and changelog

- **Latest release:** https://github.com/CinderZhang/kiss-tmux/releases/latest
- **Changelog:** [`CHANGELOG.md`](./CHANGELOG.md)
- **Development history:** [`docs/development-history.md`](./docs/development-history.md) — how the project was designed, built, and iterated on

## License

MIT

## Acknowledgements

Built with [Claude Code](https://claude.com/claude-code). The design spec ([`docs/design.md`](./docs/design.md)) and the full implementation plan ([`docs/superpowers/plans/2026-04-08-kiss-tmux.md`](./docs/superpowers/plans/2026-04-08-kiss-tmux.md)) are both in the repo. See [`docs/development-history.md`](./docs/development-history.md) for the full story including the bugs found and the lessons learned.
