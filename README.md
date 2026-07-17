# Moona

Windows-native web terminal for driving local agent CLIs (Claude Code, Codex, PowerShell, and so on) from a phone browser.

## What it does

Moona runs a small background **daemon** that holds your terminal sessions and serves them to a phone browser with xterm.js over WebSocket. Each session is a Windows ConPTY pseudoconsole. On the phone, every session shows up as a switchable **tab**.

```text
Phone browser -> temporary tunnel -> moona daemon -> ConPTY #1 (claude)
                                                  -> ConPTY #2 (pwsh)
                                                  -> ...
Windows Terminal (moona claude / moona attach) ---^  (same sessions, locally)
```

You start work the way you normally would, with `moona claude`, and it opens right away. Behind the scenes it asks the daemon to spawn the session and attaches your current terminal to it. The QR code and link live in the daemon rather than competing with your TUI for the screen, so there is no "press Enter to continue" step.

Sessions live in the daemon, so closing a terminal does not kill its session. Reconnect from any tab with `moona attach <id>`, and your phone stays connected the whole time. A session only goes away once nothing is using it: when it has had no attached terminal and no open phone/browser tab for a couple of minutes, the daemon closes it on its own so abandoned terminals don't pile up. A page you still have open keeps all of its tabs alive, so this only reaps sessions you have genuinely walked away from.

## Install

Windows PowerShell one-liner (installs or upgrades):

```powershell
irm https://raw.githubusercontent.com/hoangvu12/moona/master/install.ps1 | iex
```

This downloads the latest release, verifies its SHA256 checksum, installs `moona.exe` to `%LOCALAPPDATA%\Programs\moona`, and adds it to your user PATH. Open a new terminal, then run `moona claude`.

To pin a version, save the script and pass `-Version`:

```powershell
irm https://raw.githubusercontent.com/hoangvu12/moona/master/install.ps1 -OutFile install.ps1
.\install.ps1 -Version v0.1.0
```

## Quick start

Run a command straight into a session:

```powershell
moona claude
```

On first use Moona runs a short setup wizard (see below), auto-starts the background daemon, spawns a `claude` session, and attaches your terminal to it. To see the phone link and QR, run the daemon in a spare tab so the QR stays on screen:

```powershell
moona daemon
```

Or reprint it from any tab:

```powershell
moona qr      # QR code for the best phone URL
moona url     # just the link(s)
```

Scan the QR or open the link on your phone. The web page shows one tab per session; tap to switch which terminal you are viewing. Tap **＋** in the tab bar to start a new session from the phone (default shell, or type a command like `claude`), so you do not have to walk back to the PC.

## The dashboard

Run `moona` with no arguments to open a keyboard-driven dashboard (a TUI) over the daemon. It ensures a daemon is running, then lists your sessions so you can attach, spawn, and kill them without retyping commands:

```powershell
moona            # open the dashboard
moona ui         # same thing
```

| Key | Action |
|-----|--------|
| `↑` `↓` | move the selection |
| `↵` or `a` | attach to the selected session (Ctrl-] detaches; the session keeps running) |
| `n` | new session (attaches on start) |
| `x` or `d` | kill the selected session |
| `c` | show the phone QR code |
| `r` | refresh now (it also refreshes every second) |
| `?` | help |
| `q` | quit the dashboard (the daemon and its sessions keep running) |

Attaching hands the real terminal to `moona attach` through the same path a native terminal uses, so ConPTY sizing behaves identically.

## Choosing how your phone connects (`moona setup`)

By default the phone link is a **Cloudflare Quick Tunnel**, whose URL changes every time the daemon restarts. That is fine for a first try but awkward to bookmark. Run the one-time wizard to pick something permanent:

```powershell
moona setup
```

| Option | Permanent URL? | What it needs |
|--------|----------------|---------------|
| **Quick Tunnel** | no (new URL each restart) | nothing; the zero-setup default |
| **ngrok** | yes | a free ngrok account. Paste one authtoken, plus a reserved `*.ngrok-free.app` domain for a stable link |
| **Cloudflare (named)** | yes | a domain you own on Cloudflare. Moona authorizes Cloudflare (one browser click, skipped if already done), then creates the tunnel and DNS record for you |
| **Local Wi-Fi only** | n/a | nothing; the phone must be on the same network |

Your choice is saved to `%LOCALAPPDATA%\moona\config.json` and reused on every start, so the phone URL stops changing once you set it. For the public options Moona also generates an app token, so the link is never an unauthenticated shell. Re-run `moona setup` any time to switch providers, then restart the daemon to apply.

## Everyday commands

```powershell
moona claude                 # start Claude Code in a new session, attach this terminal
moona codex                  # any command works: moona <command> [args...]
moona pwsh.exe -NoLogo       # a raw shell
moona share                  # start the default shell in a session and attach
moona share -- codex         # explicit command after --
moona share --cmd "pwsh.exe -NoLogo"

moona                        # open the session dashboard (TUI)
moona setup                  # choose how your phone connects (stable link, etc.)
moona ls                     # list active sessions (id, clients, size, command)
moona attach 2               # attach this terminal to session 2 (reconnect after closing a tab)
moona kill 2                 # end session 2

moona daemon                 # run the switchboard in the foreground (keeps the QR on screen)
moona daemon stop            # stop the daemon and all its sessions
moona status                 # is a daemon running? how many sessions?
moona update                 # update to the latest release
```

Several terminals can each run `moona claude` (or `moona pwsh`, and so on), and all of their sessions appear as tabs on the phone. You can drive the same session from your real terminal and from the phone at once.

## The daemon

The daemon is a tiny background process (a few MB of RAM, near 0% CPU when idle). You rarely start it yourself: the first `moona <command>` auto-starts it, and it idle-exits after 15 minutes with no sessions and nothing connected. Run `moona daemon` yourself when you want the QR and link to stay visible in a dedicated tab. A daemon you start by hand does not idle-exit until you run `moona daemon stop` (or press Ctrl+C).

Discovery works through a small state file at `%LOCALAPPDATA%\moona\daemon.json` that records the daemon's port, token, and URLs, so every `moona` command can find it.

## Phone access (tunnel + QR)

With the default Quick Tunnel, the daemon tries these providers in order and prints the temporary public HTTPS URL plus a QR code:

1. Cloudflare Quick Tunnel (`*.trycloudflare.com`, auto-downloads `cloudflared.exe` if needed)
2. Pinggy (`*.pinggy.link`, non-interactive only)
3. localhost.run (`*.lhr.life`)

These URLs are ephemeral. Run [`moona setup`](#choosing-how-your-phone-connects-moona-setup) to switch to ngrok or a Cloudflare named tunnel for a URL that stays the same. Keep the daemon running while you use the link.

To stay local-only with no public tunnel:

```powershell
moona daemon --tunnel=false
moona --tunnel=false claude    # auto-starts the daemon with tunnelling off
```

For a named, stable URL with access control, point Cloudflare Tunnel and Access at the daemon's local port:

```powershell
cloudflared tunnel --url http://127.0.0.1:8787
```

## Optional app token

Cloudflare Access should be your main protection layer. For an extra app-level token, set it on the daemon:

```powershell
$env:MOONA_TOKEN = "change-me"
moona daemon
```

Clients read the token from the daemon state file, and the web link and QR include it. To attach a remote daemon with an explicit token:

```powershell
moona attach --url http://127.0.0.1:8787 --token change-me
```

## Updating

Moona keeps itself current. On startup it checks GitHub at most once per day, and if a newer release exists it verifies and installs it, then relaunches.

```powershell
moona update
moona update --check
$env:MOONA_NO_UPDATE = "1"   # opt out of the startup check
```

## Build from source

```powershell
go build -o moona.exe ./cmd/moona
```

A plain `go build` produces a `dev` build, which disables the self-updater. Released binaries are built by [GoReleaser](https://goreleaser.com) through GitHub Actions on every `v*` tag. For the hot-reload dev loop (isolated sandbox on port 8899, browser auto-reload, Air rebuilds), see [DEV.md](DEV.md).

## Notes and limitations

- Native Windows only for this MVP. It needs Windows 10 1809+ or Windows 11 for ConPTY.
- Moona spawns each session's process itself under a ConPTY. Windows has no way to adopt an already-running console (a `claude` you started in a plain tab before Moona) into a pseudoconsole. Start work through `moona <command>` (or make a "Moona pwsh" Windows Terminal profile) so every session is shareable from the start.
- Temporary tunnels are third-party services, so availability can vary. Use `--tunnel=false` for local-only.
- The browser assets currently load xterm.js from the jsDelivr CDN.
- Anyone who can reach the web terminal can run commands on your machine. Keep it behind Cloudflare Access or a VPN, and bound to `127.0.0.1`, unless you know what you are doing.
