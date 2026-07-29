# dterm

A terminal session manager built in Go. SSH bookmarks, split panes, SFTP browser, tunnel manager — in a single binary.

## Why this exists

I spend most of my day SSH'd into air-gapped OpenShift clusters, jumping between bastion hosts, worker nodes, and CI runners. MobaXterm does the job but it's closed source. I went looking for an open-source replacement that could handle SSH bookmarks, split terminals, and SFTP in one window.

Here's what I found:

- **Tabby / Electerm** — feature-complete, but Electron. 200MB of RAM to hold a terminal. On a locked-down workstation with 30 tabs open, that matters.
- **WindTerm** — native C++, fast, lightweight. Tried it. The menu bar disappeared. Right-click stopped working. PowerShell 7 output turned into a wall of `ESC[0K`. Filed under: promising but fragile.
- **WezTerm** — great terminal emulator, but no session bookmarks, no SFTP browser, no tunnel manager. It emulates a terminal. I need a session manager.
- **PuTTY + WinSCP** — the classic combo. Two windows, two configs, zero integration. It works the way duct tape works.

Nothing fit. Everything was either too heavy, too broken, or too split across tools.

So I built one.

## What it is

`dterm` is a TUI session manager that runs inside any terminal. It is not a terminal emulator — it's what runs inside one. You point it at a YAML file of bookmarks, pick a host, and you're in. Split the screen, open another host beside it, browse files over SFTP, forward ports — all without leaving the window.

One Go binary. No Electron. No JRE. No runtime dependencies.

## What it does

- **SSH bookmarks** — YAML config, grouped by environment, searchable. No clicking through connection wizards.
- **Split panes** — vertical or horizontal. Click to focus, navigate spatially with `Ctrl+B hjkl`. Open as many sessions as your screen fits.
- **Full VT100/xterm emulation** — vim, htop, tmux, ncurses apps all render correctly inside panes.
- **Sync input** — broadcast every keystroke to all panes simultaneously. Run a command on 10 hosts at once.
- **SFTP browser** — dual-pane file manager (local ↔ remote), upload/download with progress, tab-completion for paths, optional datetime column, last-directory memory per host.
- **SSH tunnel manager** — local, remote, and dynamic (SOCKS5) port forwards. Tunnel status badges on pane borders. Tunnels persist across sessions when saved to config.
- **Togglable timestamps** — prepend `HH:MM:SS` to every output line. Useful for tailing logs across hosts and correlating events.
- **Text selection** — drag to select, double-click for word, triple-click for line. Auto-copies on release. Right-click or `Shift+Insert` to paste.
- **Mouse support** — click to select bookmarks, click to focus panes, scroll through scrollback (2000 lines).
- **Multiple auth methods** — SSH agent (Windows named pipe or Unix socket), key file with optional passphrase, password with TUI prompt. Trust-on-first-use known_hosts.
- **Minimal footprint** — single static binary, ~10MB compiled. Runs on Linux, macOS, Windows.

## Quick start

```bash
go build -o dterm .
./dterm
```

Config is read from `~/.dterm/bookmarks.yaml` by default:

```bash
./dterm -config /path/to/bookmarks.yaml
```

## Config

```yaml
bookmarks:
  - name: dev-bastion
    host: 10.10.1.100
    port: 22
    user: admin
    key_file: ~/.ssh/id_ed25519
    group: dev

  - name: prod-master-01
    host: 10.20.1.10
    user: core
    key_file: ~/.ssh/id_ed25519
    group: prod
    tunnels:
      - name: api
        type: local
        local_port: 8080
        remote_host: internal-api.svc
        remote_port: 80
      - name: socks
        type: dynamic
        local_port: 1080
```

Fields per bookmark:

| Field | Default | Description |
|---|---|---|
| `name` | required | Display name |
| `host` | required | Hostname or IP |
| `port` | `22` | SSH port |
| `user` | required | SSH username |
| `key_file` | — | Path to private key (`~` and `$ENV` expanded) |
| `password` | — | Password or key passphrase |
| `group` | — | Group label for the bookmark list |
| `tunnels` | — | Port forwards to register on connect |

Tunnel types: `local` (`-L`), `remote` (`-R`), `dynamic` (`-D`).

## Keybindings

### Bookmark screen

| Key | Action |
|---|---|
| `j` `k` / `↑` `↓` | Navigate list |
| `h` `l` / `←` `→` | Previous / next group (group view) |
| `Tab` / `Shift+Tab` | Cycle groups |
| `g` | Toggle All / Group view |
| `/` | Search bookmarks |
| `Enter` | Connect |
| `q` / `Ctrl+C` | Quit |

### Terminal — `Ctrl+B` prefix (tmux-style)

| Chord | Action |
|---|---|
| `Ctrl+B %` | Vertical split |
| `Ctrl+B "` | Horizontal split |
| `Ctrl+B h` `j` `k` `l` / arrows | Focus pane (spatial) |
| `Ctrl+B o` / `n` | Cycle to next pane |
| `Ctrl+B x` | Close active pane |
| `Ctrl+B b` / `Space` | Back to bookmark list |
| `Ctrl+B t` | Toggle per-line timestamps |
| `Ctrl+B s` | Toggle sync input (broadcast to all panes) |
| `Ctrl+B f` | Open SFTP browser |
| `Ctrl+B T` | Open tunnel manager |
| `Ctrl+B q` | Quit |
| `Ctrl+B Ctrl+B` | Send literal `^B` to SSH |
| `Ctrl+Q` | Quit (global, works anywhere) |

### Mouse

| Action | Result |
|---|---|
| Click pane | Focus pane |
| Drag | Select text (copies on release) |
| Double-click | Select word |
| Triple-click | Select line |
| Right-click | Paste clipboard |
| Scroll | Scroll pane output / scrollback |
| `Shift+Insert` | Paste clipboard |
| `Ctrl+Insert` | Copy active selection |

### SFTP browser

| Key | Action |
|---|---|
| `Tab` | Switch active panel |
| `↑` `↓` / `j` `k` | Navigate entries |
| `Enter` | Open directory / download to local panel |
| `u` | Upload local selection to remote |
| `Backspace` | Go up one directory |
| `T` | Toggle datetime column |
| `/` | Filter entries |
| `Esc` | Close SFTP browser |

Path bar supports `Tab` completion.

### Scrollback

| Key | Action |
|---|---|
| `PgUp` / `PgDn` | Scroll up / down |
| Scroll wheel | Scroll 3 lines |

New output does not snap the viewport while you are scrolled up.

## Built with

- [Bubble Tea](https://github.com/charmbracelet/bubbletea) — TUI framework
- [Lip Gloss](https://github.com/charmbracelet/lipgloss) — styling
- [Bubbles](https://github.com/charmbracelet/bubbles) — TUI components
- [x/crypto/ssh](https://pkg.go.dev/golang.org/x/crypto/ssh) — SSH client
- [pkg/sftp](https://github.com/pkg/sftp) — SFTP client

## License

MIT
