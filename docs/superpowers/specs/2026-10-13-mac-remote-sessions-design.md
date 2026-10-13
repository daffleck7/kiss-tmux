# Mac Remote Sessions Design Spec

**Date:** 2026-10-13
**Status:** Approved (pending spec review)

## Goal

Control Claude Code sessions running on a headless Mac Mini from the KISS-TMUX UI on
Windows, alongside local Windows sessions. Sessions are grouped into per-machine tabs and
color-coded so it is always obvious which machine you are typing into.

## Context

The Mac already has a `cs` command (`/Users/me/.local/bin/cs`) wrapping GNU `screen`:

| Command | Effect |
|---------|--------|
| `cs` | List running sessions; start "main" if none |
| `cs <name> <dir>` | Start session `<name>` in `<dir>` |
| `cs <name>` | Reattach to `<name>` |
| `cs -k <name>` | End `<name>` |

`screen` sessions run detached in the background; no terminal needs to be open on the Mac.
The Mac must stay awake and not reboot (a reboot ends all `screen` sessions).

Prerequisites (outside this codebase):
- Mac: System Settings → General → Sharing → Remote Login **on** (user does this at the Mac).
- Windows: `~/.ssh/config` with a `mac` host entry and an SSH key installed on the Mac.

## Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Transport | Run system `ssh.exe` inside ConPTY | Reuses all existing terminal plumbing; honors `~/.ssh/config` and keys |
| Persistence | Mac-side `screen` via `cs` | Already set up; survives KISS-TMUX restarts and Wi-Fi drops |
| UI grouping | One tab per machine (Windows, Mac) | Clear separation; each tab has its own grid/focus view |
| Color | Per-machine accent color on tab, borders, sidebar | Instant visual cue of active machine |
| Reconnect | Auto-discover on startup + auto-reconnect on drop | Mac sessions should "just be there" |
| Close semantics | Separate "Disconnect" and "End on Mac" | Don't accidentally kill long-running agents |
| Mac folder pick | Remote folder browser over SSH | Same UX as the local browser |
| Tailscale | Not now | Host lives in `~/.ssh/config`; can be repointed with no code change |
| Native Go SSH | Rejected | More code (keys, known_hosts, config) for no visible benefit |
| Run kiss-tmux-mac on Mac | Rejected | Two servers, duplicate persistence layer |

## Configuration

`config.json` (next to the exe) gains:

```json
{
  "default_cwd": "C:\\Users\\you\\Desktop",
  "local_label": "Windows",
  "local_color": "#3b82f6",
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

- `local_label` defaults to `"Windows"`, `local_color` to `#3b82f6`.
- `hosts` defaults to empty → UI shows only the Windows tab (current behavior).
- `id` must match `^[a-z0-9_-]+$`. `ssh` is the alias passed to `ssh.exe`.
- `/api/config` returns the full config so the frontend can build tabs.

## Backend

### Session model

`Session` and `SessionInfo` gain:
- `Host string` — `""` for local, otherwise a host `id`.
- `State string` — `"running" | "reconnecting" | "disconnected" | "ended" | "exited"`.
  Local sessions use only `running`/`exited`. `Running bool` is kept for compatibility
  (`true` iff state is `running`).

- `RemoteName string` — the `screen` session name used with `cs`. Must match
  `^[A-Za-z0-9_-]+$` (validated on spawn; the UI slugifies the typed name, default
  `s<id>`). It never changes after spawn; Rename changes only the display `Name`.
  Discovery matches existing sessions by `(Host, RemoteName)`.

### Commands (new file `remote.go`)

Pure, unit-tested builders:
- `attachCmd(host, name, cwd)` → `ssh -t -o ServerAliveInterval=30 <alias> cs <name> <cwd>`
  (cwd omitted on reattach). `cwd` is POSIX single-quoted for the remote shell; a leading
  `~/` stays outside the quotes so it expands (e.g. `~/'Desktop/My Projects'`).
  `cs <name> <dir>` reattaches if `<name>` exists (ignoring `<dir>`); a missing `<dir>` on a
  new session exits 1 with `No such folder: <dir>`, shown in that session's terminal; the session then goes to `ended` (the name is absent from `screen -ls`).
- `listCmd(host)` → `ssh -o BatchMode=yes -o ConnectTimeout=5 <alias> screen -ls`
- `endCmd(host, name)` → `ssh -o BatchMode=yes <alias> cs -k <name>`
- `browseCmd(host, path)` → `ssh -o BatchMode=yes <alias>` running a small `sh` snippet that
  prints the resolved absolute path then one directory name per line (hidden dirs skipped).
  `~` and `~/...` are expanded by the remote shell; everything else is single-quoted.

`parseScreenList(output)` extracts session names from `screen -ls` lines of the form
`\t<pid>.cs-<name>\t(<date>)\t(Detached|Attached)`. `cs` prefixes screen names with `cs-`;
only `cs-` sessions are kept (others ignored) and the prefix is stripped. `cs` with no args
is not used for discovery because it auto-starts "main" when none exist. `cs` does not
validate names, so KISS-TMUX enforces `^[A-Za-z0-9_-]+$`.

One-off commands (`list`, `end`, `browse`) run via `exec.CommandContext` with a 10s timeout.
`BatchMode=yes` means they fail fast with a clear error instead of hanging on a password
prompt. The attach command runs in ConPTY (interactive), so a password prompt there still
works if no key is installed.

### Discovery

On startup and on a `discover` client message (sent when the user opens a host tab), the
manager runs `listCmd` for each host. For each listed name with no existing session for that
host, it spawns an attach session (state `running`). Discovery errors are broadcast as an
`error` message with the host id (shown as a banner in that tab).

### Reconnect state machine (remote sessions only)

When a remote session's ConPTY read loop ends:
1. If `disconnectRequested` → state `disconnected`. Stop.
2. If `endRequested` → state `ended`. Stop.
3. Otherwise → state `reconnecting`; run `listCmd`:
   - name present → respawn attach after backoff (1s, 2s, 4s … capped at 30s; reset to 1s
     after a connection stays up 60s).
   - name absent (session ended on the Mac) → state `ended`. Stop.
   - list fails (network down) → retry after backoff.

The reconnect loop runs in a goroutine per session and stops if the session is removed.
Reconnecting reuses the same session ID and ring buffer, so the xterm instance keeps its
scrollback. Each transition broadcasts `sessions`.

A `reconnect` client message on a `disconnected` session restarts attach immediately.

### Protocol additions

Client → server:
- `spawn` gains `host` (optional).
- `disconnect {session}` — close SSH, leave Claude running on the Mac.
- `end {session}` — run `endCmd`, then close.
- `reconnect {session}` — reattach a disconnected session.
- `discover {host}` — re-scan a host.

Existing `kill` stays for local sessions; for remote sessions the server treats it as
`disconnect`. `remove` on a remote session disconnects and drops it from the UI (Claude
keeps running; it reappears on the next discover).

### Browse

`/api/browse?host=<id>&path=<p>` runs `browseCmd` and returns the same JSON shape as the
local browser (`path`, `parent`, `entries`, `drives: []`). No `host` → existing local
behavior.

### Max sessions

The cap of 8 applies per machine.

## Frontend

- **Tab bar** across the top: one tab per machine (`■ WINDOWS (3)`, `■ MAC (2)`), the square
  and active underline in the machine's color. Active tab remembered in `localStorage`.
- Grid and focus views render only the active machine's sessions. Grid cell borders, the
  focus sidebar accent, and the focused terminal's border use the machine's color via a
  `--host-color` CSS variable set on the view.
- Remote session headers show state: a dim "reconnecting…" / "disconnected" / "ended" badge.
  Disconnected sessions show a **Reconnect** button.
- **New Session dialog** creates on the active machine. For a remote machine: the Command
  field is hidden (it is always `cs`), the Name field is required-ish (slugified, default
  generated), and the folder browser calls `/api/browse?host=<id>`, starting at the host's
  `default_cwd`.
- **Context menu** for remote sessions: Rename (UI label only), Disconnect, End on Mac…
  (with `confirm()`), Remove. Local sessions keep Rename / Kill / Remove.
- Opening a host tab sends `discover {host}`.
- Host-level error banner (e.g. "Can't reach mac: Permission denied (publickey)") at the top
  of the tab, dismissible.

## One-time Windows setup (not code)

1. Create `C:\Users\you\.ssh\config`:
   ```
   Host mac
       HostName my-mac.local
       User me
       RequestTTY yes
       ServerAliveInterval 30
       ServerAliveCountMax 3
   ```
2. `ssh-keygen -t ed25519` (if no key), then copy `id_ed25519.pub` into the Mac's
   `~/.ssh/authorized_keys` (one-time password prompt).
3. Add the `hosts` entry to `config.json`.

## Build & deploy

Rebuild with `go build -o kiss-tmux.exe` in place. `kiss-tmux.ico`, `kiss-tmux.png`, and
`Desktop\KISS-TMUX.lnk` must not be touched or deleted.

## Testing

Unit (Go, `go test ./...`):
- `parseScreenList` against real-format fixtures (attached, detached, none, "No Sockets", non-`cs-` sessions ignored).
- Command builders: quoting of names/paths with spaces and quotes; `~` handling.
- Config loading: defaults, hosts, invalid host id rejected.
- Reconnect decision logic as a pure function `nextAction(flags, listResult) → action`.
- Backoff sequence and reset.
- Remote name validation.

Manual (after Remote Login is enabled):
- Create Mac session, browse folders, type into Claude.
- Restart KISS-TMUX → Mac sessions rediscovered.
- Disable Wi-Fi briefly → session shows reconnecting, then resumes.
- Disconnect → Claude still running (`screen -ls`); Reconnect works.
- End on Mac → session gone from `screen -ls`.
- Verify Claude auth works over SSH (macOS Keychain may be locked in SSH sessions).

## Out of scope

- Tailscale, multiple remote hosts in practice (config supports it, only Mac is tested).
- Installing tmux on the Mac.
- Changing the Mac's `cs` script.
