# Changelog

All notable changes to KISS-TMUX are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Remote machine tabs.** Run and monitor Claude sessions on a Mac (or any SSH host with a `cs` + `screen` wrapper) alongside local sessions.
- Per-machine color coding on tabs, grid borders, sidebar, focused terminal and dialog.
- Auto-discovery of running remote sessions on startup, on tab open and on browser reconnect.
- Auto-reconnect with exponential backoff (1s to 30s); stops when the remote session is gone.
- Separate **Disconnect** (keep running) and **End on remote** (`cs -k`, confirmed) actions.
- Remote folder browser over SSH; error banner and tab alert when a machine is unreachable.
  The banner is also shown to browser tabs opened after the error (e.g. a startup
  "can't reach"), and clears itself once the machine answers again.
- Reconnect bar in focus view for a disconnected remote session.
- `hosts`, `local_label` and `local_color` in `config.json`.

### Security
- The WebSocket only accepts connections from KISS-TMUX's own page
  (`127.0.0.1:<port>` / `localhost:<port>`) or non-browser clients, so other websites can't
  drive your sessions.
- **End** checks `screen -ls` first and only runs `cs -k` for an exact name match; screen's
  prefix matching can no longer stop a different session.

### Fixed
- Sessions whose process exits now show as exited (ConPTY kept the output pipe open).
- Sidebar status dots now have colors.
- A second instance on the same port exits before attaching to remote sessions.

## [v0.3.0] — 2026-04-09

### Added
- **WebGL renderer** via `xterm-addon-webgl`. GPU-accelerated text rendering matches what VSCode and modern web-based terminals use. Smoother scrolling and crisper glyphs.
- Graceful fallback to the default renderer if WebGL is unavailable.
- Context-loss handling disposes the WebGL addon cleanly on GPU context loss (e.g., laptop sleep).

### Changed
- Extra font fallback: `Cascadia Code → Consolas → Courier New → monospace`.
- `lineHeight: 1.2` for better readability.
- Scrollback buffer doubled from 5000 to 10000 lines.
- Enabled `windowsMode: true` for better Windows line-ending and cursor handling.

[Full diff](https://github.com/CinderZhang/kiss-tmux/compare/v0.2.0...v0.3.0)

## [v0.2.0] — 2026-04-09

### Added
- **Copy/paste support** in terminals:
  - `Ctrl+V` / `Ctrl+Shift+V` / right-click paste delivers the clipboard to the active terminal via `term.paste()` (handles bracketed paste mode).
  - `Ctrl+Shift+C` copies the selected text from the active terminal.
  - `Ctrl+C` continues to send SIGINT as expected.
- README with quick start, features, architecture diagram, and build instructions.
- `go install github.com/CinderZhang/kiss-tmux@latest` documented as an install path.

### Changed
- `.gitignore` now covers all `kiss-tmux-*.exe` build artifacts.

[Full diff](https://github.com/CinderZhang/kiss-tmux/compare/v0.1.0...v0.2.0)

## [v0.1.0] — 2026-04-08

First public release. Built end-to-end in a single session from the design spec through a working Windows binary.

### Added
- **Go backend** with single binary via `go:embed`:
  - Windows ConPTY session manager with spawn/kill/rename/list/input/resize
  - 64KB ring buffer per session for reconnect replay
  - WebSocket hub with read/write pumps per client
  - HTTP server with embedded static assets
- **Web frontend** (vanilla HTML/JS, no npm):
  - Grid mode (auto-arranging CSS grid for up to 8 sessions)
  - Focus mode (sidebar with 6-char session abbreviations + full-size terminal)
  - Hover tooltips showing full session names
  - Sidebar buttons for grid toggle (☰) and new session (+)
  - New session dialog with name/cwd/command fields
  - Context menu (rename, kill) and inline double-click rename
  - Live reconnect with ring buffer replay
- **Default command**: `claude --dangerously-skip-permissions`
- Persistent terminal state across grid/focus switches via wrapper div reparenting
- Critical concurrency fixes:
  - `writePump` starts before hub registration to prevent deadlock
  - `safeSend()` guards against sends on closed channels
  - Window resize only fits visible terminals

### Fixed during v0.1.0 development
- Terminal content blanked on grid/focus switch — root cause was `term.open()` being called on every mode switch. Fixed by calling `open()` once into a persistent wrapper and moving the wrapper with `appendChild`.
- Terminal blanked again when switching between sessions in focus mode — stale `t.el === container` guard in `attachTerminal` short-circuited after `innerHTML=''` detached the wrapper. Fixed by always re-appending.

[Full diff](https://github.com/CinderZhang/kiss-tmux/compare/523998a...v0.1.0)

---

[v0.3.0]: https://github.com/CinderZhang/kiss-tmux/releases/tag/v0.3.0
[v0.2.0]: https://github.com/CinderZhang/kiss-tmux/releases/tag/v0.2.0
[v0.1.0]: https://github.com/CinderZhang/kiss-tmux/releases/tag/v0.1.0
