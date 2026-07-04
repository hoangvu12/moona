# Dev workflow (hot reload)

Fast inner loop for moona. Everything runs in an isolated **sandbox** (`.testhome`,
port **8899**) so it never touches the real `:8787` daemon that hosts your Claude
Code session. Delete `.testhome` to reset.

## One-time setup
```
go install github.com/air-verse/air@latest   # put your Go bin dir (e.g. %USERPROFILE%\go\bin) on PATH
```

## Run
```
.\dev.ps1          # desktop:  http://127.0.0.1:8899/
.\dev.ps1 phone    # phone:    https://moona-dev.nguyenvu.dev/?token=<dev-token>
```
Ctrl+C stops the loop. The phone URL + token are written to `.testhome/dev-url.txt`.

## What auto-happens
| You edit | Result | Rebuild? |
|---|---|---|
| `cmd/moona/web/index.html` | browser **auto-reloads** | no |
| any `.go` file | Air rebuilds `./cmd/moona` + **restarts** the sandbox daemon; tab reconnects | yes (~1s) |

- **UI hot reload**: `MOONA_DEV=1` makes the daemon serve `index.html` from disk
  (not the `//go:embed` copy) and push a `{type:"reload"}` frame over the terminal
  WebSocket → `location.reload()`. See `cmd/moona/web.go`.
- **Seed session**: `MOONA_DEV_SEED` (a shell) makes the daemon spawn one session on
  every boot, so after a rebuild the tab reconnects to a live terminal, not the
  empty state. See `seedDevSession` in `cmd/moona/web.go`.

## Phone tunnel (fixed URL)
`.\dev.ps1 phone` runs a **named** Cloudflare tunnel as its own process so the URL
stays constant across daemon *and* tunnel restarts:
```
cloudflared tunnel --url http://127.0.0.1:8899 run moona-dev
```
Created once (already done): `cloudflared tunnel create moona-dev` +
`cloudflared tunnel route dns moona-dev moona-dev.nguyenvu.dev`.
The sandbox is public, so it's token-gated: the daemon reads `MOONA_TOKEN`; the
browser takes `?token=` from the URL and saves it. **Don't share the link.**

## Gotchas
- **Never blanket-kill cloudflared** — the prod tunnel (`run moona` → `:8787`) serves
  your live session. Target by cmdline: `run moona-dev` is the dev one.
- **Never kill the real `:8787` daemon** — it hosts the Claude Code session running
  these commands. Dev is `:8899` only.
- Shipping a *real* backend change to the phone's `:8787` daemon is a separate,
  deliberate step (hot-swap relauncher), not this loop.
- Manual restart of the sandbox daemon: `POST http://127.0.0.1:8899/api/shutdown?token=<tok>`
  then relaunch (or just let Air do it on the next `.go` save).
