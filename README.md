# Moona

Windows-native web terminal MVP for using local agent CLIs (Claude Code, Codex, PowerShell, etc.) from a phone browser.

## What it does

`moona` starts a local Go web server, creates a Windows ConPTY pseudoconsole, and streams it to a browser with xterm.js over WebSocket.

```text
Phone browser -> temporary tunnel/Cloudflare Tunnel -> moona.exe -> Windows ConPTY -> pwsh/claude/etc.
```


For local desktop use, `moona attach` connects your normal Windows Terminal to the same running session:

```text
Windows Terminal -> moona attach -> moona.exe share -> Windows ConPTY -> pwsh/claude/etc.
Phone browser --------------------------------------^ 
```

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

## Updating

Moona keeps itself current. On startup it checks GitHub at most once per day and, if a newer release exists, verifies and installs it automatically, then relaunches — no admin rights needed (the binary lives in a user-writable folder).

Update on demand, or just check:

```powershell
moona update
moona update --check
```

Opt out of the automatic startup check:

```powershell
$env:MOONA_NO_UPDATE = "1"
```

## Build from source

```powershell
go build -o moona.exe ./cmd/moona
```

A plain `go build` produces a `dev` build, which disables the self-updater. Released binaries are built by [GoReleaser](https://goreleaser.com) via GitHub Actions on every `v*` tag.

## Use locally

Fast shortcut: put any command after `moona`; Moona prints the phone link/QR first, then press Enter to start it and attach your Windows Terminal:

```powershell
.\moona.exe codex
.\moona.exe opencode
.\moona.exe claude
```

Moona starts the web server and tunnel first, prints the URL/QR, then waits: press Enter to start that command and attach locally, or type `q` then Enter to quit before the command starts. Your phone/browser can still connect to the same session while this is running.


By default, Moona also tries to create a free temporary phone link and QR code:

```powershell
.\moona.exe codex
```

This starts a free temporary tunnel, prints the public HTTPS URL, renders a scannable QR code in the terminal, and waits for your Enter/q choice before launching the command. Moona uses Cloudflare Quick Tunnel first and auto-downloads `cloudflared.exe` into your user cache if it is not already installed. Pinggy is only tried without password prompts, and localhost.run is last fallback. If you only want local terminal/browser access, use `--tunnel=false`.

Start a PowerShell-backed terminal:

```powershell
.\moona.exe share
```


In another Windows Terminal tab/window, attach locally:

```powershell
.\moona.exe attach
```

Now you can use the managed session from your real terminal and from the website at the same time.

Open:

```text
http://127.0.0.1:8787
```

Manual two-step agent flow:
```powershell
.\moona.exe share -- codex
```
Then attach from a normal terminal if you do not want to type in the browser:

```powershell
.\moona.exe attach
```

You can use the same shortcut for other commands:

```powershell
.\moona.exe codex
.\moona.exe pwsh.exe -NoLogo
```

Or use a raw command line:

```powershell
.\moona.exe share --cmd "pwsh.exe -NoLogo"
```

The terminal session persists across browser refreshes/disconnects while `moona.exe share` keeps running. `moona attach` is just another client for that same session.

## Quick phone access with temporary tunnel + QR

Tunnel + QR are on by default, so this is enough:

```powershell
.\moona.exe codex
```

or manually:

```powershell
.\moona.exe share -- codex
```

Moona tries these tunnel providers in order: Cloudflare Quick Tunnel (`*.trycloudflare.com`, auto-downloads `cloudflared.exe` if needed), Pinggy (`*.pinggy.link`, non-interactive only), then localhost.run (`*.lhr.life`). It prints the temporary public HTTPS URL and a QR code you can scan from your phone. In shortcut mode (`moona <anything>`), the command does not start until you press Enter, so the QR stays visible and clean. Keep Moona running while you use the link.

Skip the temporary tunnel and stay local-only with:

```powershell
.\moona.exe --tunnel=false codex
.\moona.exe share --tunnel=false
```


Disable QR output with:

```powershell
.\moona.exe share --qr=false --tunnel
```

## Phone access with Cloudflare Tunnel

For a named/stable URL and access control, Cloudflare Tunnel + Access is still a good option. Install `cloudflared`, then after `moona` is running:

```powershell
cloudflared tunnel --url http://127.0.0.1:8787
```

Then protect the public hostname with Cloudflare Access.

## Optional app token

Cloudflare Access should be the main protection layer. For an extra app-level WebSocket token:

```powershell
$env:MOONA_TOKEN = "change-me"
.\moona.exe share
```


Attach with the same token:

```powershell
.\moona.exe attach --token change-me
```

Open:

```text
http://127.0.0.1:8787/?token=change-me
```

## Notes / limitations

- Native Windows only for this MVP.
- Requires Windows 10 1809+ or Windows 11 for ConPTY.
- Temporary tunnel + QR are enabled by default. Use `--tunnel=false` for local-only startup.
- The default tunnel uses Cloudflare Quick Tunnel first and may download `cloudflared.exe` to your user cache. Pinggy is tried without password prompts; localhost.run is the last fallback. These are third-party services, so availability can vary.
- This does not take over an already-running Windows Terminal tab. Start important work through `moona share`, then use `moona attach` from a terminal tab if you prefer local terminal control.
- The browser assets currently load xterm.js from jsDelivr CDN.
- Anyone who can access the web terminal can run commands on your machine; keep it behind Cloudflare Access/VPN and bind to `127.0.0.1` unless you know what you are doing.
