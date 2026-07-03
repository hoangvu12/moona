# Moona

Windows-native web terminal for using local agent CLIs (Claude Code, Codex, PowerShell, etc.) from a phone browser.

## What it does

Moona runs a small background **daemon** that holds one or more terminal sessions and serves them to a phone browser with xterm.js over WebSocket. Each session is a Windows ConPTY pseudoconsole. The phone sees every session as a switchable **tab**.

```text
Phone browser -> temporary tunnel -> moona daemon -> ConPTY #1 (claude)
                                                  -> ConPTY #2 (pwsh)
                                                  -> ...
Windows Terminal (moona claude / moona attach) ---^  (same sessions, locally)
```

You start work the same way you always would — `moona claude` — and it opens **instantly**. Under the hood it asks the daemon to spawn the session and attaches your current terminal to it. Because the QR/link live in the daemon (not fighting your TUI for the screen), there is no "press Enter to continue" step anymore.

Sessions live in the daemon, so **closing a terminal does not kill its session** — reconnect from any tab with `moona attach <id>`, and your phone keeps working the whole time.

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

```powershell
moona claude
```

That is it. On first use Moona auto-starts the background daemon, spawns a `claude` session, and attaches your terminal to it. To see the phone link/QR, either run the daemon yourself in a spare tab (so the QR stays on screen):

```powershell
moona daemon
```

or reprint it any time from any tab:

```powershell
moona qr      # QR code for the best phone URL
moona url     # just the link(s)
```

Scan the QR (or open the link) on your phone. The web page shows a tab per session; tap to switch which terminal you are viewing. Tap **＋** in the tab bar to start a new session (default shell, or type a command like `claude`) straight from the phone — no need to walk back to the PC.

## Choosing how your phone connects (`moona setup`)

By default the phone link is a **Cloudflare Quick Tunnel**, whose URL changes every time the daemon restarts — great for a first try, annoying to bookmark. Run the one-time wizard to pick something permanent:

```powershell
moona setup
```

| Option | Permanent URL? | What it needs |
|--------|----------------|---------------|
| **Quick Tunnel** | no (new URL each restart) | nothing — the zero-setup default |
| **ngrok** | yes | a free ngrok account: paste one authtoken, plus a reserved `*.ngrok-free.app` domain for a stable link |
| **Cloudflare (named)** | yes | a domain you own on Cloudflare — moona does the rest: it authorizes Cloudflare (one browser click, skipped if already done), then creates the tunnel and DNS record for you automatically |
| **Local Wi-Fi only** | n/a | nothing; phone must be on the same network |

Your choice is saved to `%LOCALAPPDATA%\moona\config.json` and reused on every start, so once set the phone URL stops changing. For the public options moona also generates an app token automatically, so the link is never an unauthenticated shell. Re-run `moona setup` any time to change providers (restart the daemon to apply).

## Everyday commands

```powershell
moona claude                 # start Claude Code in a new session, attach this terminal
moona codex                  # any command works: moona <command> [args...]
moona pwsh.exe -NoLogo       # a raw shell
moona share                  # start the default shell in a session and attach
moona share -- codex         # explicit command after --
moona share --cmd "pwsh.exe -NoLogo"

moona setup                  # choose how your phone connects (stable link, etc.)
moona ls                     # list active sessions (id, clients, size, command)
moona attach 2               # attach this terminal to session 2 (reconnect after closing a tab)
moona kill 2                 # end session 2

moona daemon                 # run the switchboard in the foreground (keeps the QR on screen)
moona daemon stop            # stop the daemon and all its sessions
moona status                 # is a daemon running? how many sessions?
```

Multiple terminals can each run `moona claude` (or `moona pwsh`, etc.) and all their sessions show up as tabs on the phone. Use the same session from your real terminal and from the phone at the same time.

## The daemon

The daemon is a tiny background process (a few MB of RAM, ~0% CPU when idle). You normally never start it yourself — the first `moona <command>` auto-starts it and it **idle-exits** after 15 minutes with no sessions and nothing connected. Run `moona daemon` yourself only when you want the QR/link to stay visible in a dedicated tab; a manually started daemon does not idle-exit until you `moona daemon stop` (or press Ctrl+C).

Discovery is a small state file at `%LOCALAPPDATA%\moona\daemon.json` that records the daemon's port, token, and URLs so every `moona` command can find it.

## Phone access (tunnel + QR)

By default (Quick Tunnel) the daemon tries these providers in order and prints the temporary public HTTPS URL + QR: Cloudflare Quick Tunnel (`*.trycloudflare.com`, auto-downloads `cloudflared.exe` if needed), Pinggy (`*.pinggy.link`, non-interactive only), then localhost.run (`*.lhr.life`). These URLs are ephemeral — run [`moona setup`](#choosing-how-your-phone-connects-moona-setup) to switch to ngrok or a Cloudflare named tunnel for a URL that stays the same. Keep the daemon running while you use the link.

Stay local-only (no public tunnel) with:

```powershell
moona daemon --tunnel=false
moona --tunnel=false claude    # auto-starts the daemon with tunnelling off
```

For a named/stable URL and access control, use Cloudflare Tunnel + Access against the daemon's local port:

```powershell
cloudflared tunnel --url http://127.0.0.1:8787
```

## Optional app token

Cloudflare Access should be the main protection layer. For an extra app-level token, set it on the daemon:

```powershell
$env:MOONA_TOKEN = "change-me"
moona daemon
```

Clients pick the token up from the daemon state file automatically; the web link/QR include it. To attach a remote daemon with an explicit token:

```powershell
moona attach --url http://127.0.0.1:8787 --token change-me
```

## Updating

Moona keeps itself current. On startup it checks GitHub at most once per day and, if a newer release exists, verifies and installs it automatically, then relaunches.

```powershell
moona update
moona update --check
$env:MOONA_NO_UPDATE = "1"   # opt out of the startup check
```

## Build from source

```powershell
go build -o moona.exe ./cmd/moona
```

A plain `go build` produces a `dev` build, which disables the self-updater. Released binaries are built by [GoReleaser](https://goreleaser.com) via GitHub Actions on every `v*` tag.

## Notes / limitations

- Native Windows only for this MVP. Requires Windows 10 1809+ or Windows 11 for ConPTY.
- Moona spawns each session's process itself under a ConPTY; Windows has no way to adopt an **already-running** console (a `claude` you started in a plain tab before Moona) into a pseudoconsole. Start work through `moona <command>` (or make a "Moona pwsh" Windows Terminal profile) so every session is shareable from the start.
- Temporary tunnels are third-party services, so availability can vary. Use `--tunnel=false` for local-only.
- The browser assets currently load xterm.js from jsDelivr CDN.
- Anyone who can reach the web terminal can run commands on your machine; keep it behind Cloudflare Access/VPN and bound to `127.0.0.1` unless you know what you are doing.
