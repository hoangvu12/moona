# test-run.ps1 — run the freshly built moona.exe in a fully isolated sandbox so it
# never touches the daemon that is hosting your current Claude Code session
# (which is squatting on the default port 8787). Uses a private state dir and a
# non-default port, so the wizard/daemon/config here are separate from the real ones.
#
# Usage:
#   .\test-run.ps1                 # first run -> setup wizard, then dashboard
#   .\test-run.ps1 setup           # re-run the setup wizard
#   .\test-run.ps1 claude          # start a claude session in the sandbox
#   .\test-run.ps1 daemon          # run the daemon in the foreground (keeps QR on screen)
#   .\test-run.ps1 ls              # list sandbox sessions
#
# Everything lives under .\.testhome (gitignored). Delete that folder to reset.

param([Parameter(ValueFromRemainingArguments = $true)] $Rest)

$ErrorActionPreference = 'Stop'
$root    = $PSScriptRoot
$exe     = Join-Path $root 'moona.exe'
$sandbox = Join-Path $root '.testhome'
$port    = 8899

if (-not (Test-Path $exe)) {
  Write-Host "moona.exe not found - build it first:  go build -o moona.exe .\cmd\moona" -ForegroundColor Yellow
  exit 1
}
New-Item -ItemType Directory -Force -Path $sandbox | Out-Null

# Isolate ALL moona state (daemon.json, config.json, cloudflared/ngrok cache, logs).
$env:LOCALAPPDATA = $sandbox
$env:MOONA_NO_UPDATE = '1'
# Serve web/index.html from disk + auto-reload the browser on save (UI hot-reload).
# Absolute path so it resolves even in the auto-spawned detached daemon.
$env:MOONA_DEV = '1'
$env:MOONA_DEV_WEB = Join-Path $root 'cmd\moona\web\index.html'

$sub  = if ($Rest.Count -gt 0) { $Rest[0] } else { '' }
$tail = if ($Rest.Count -gt 1) { $Rest[1..($Rest.Count - 1)] } else { @() }

Write-Host "sandbox: $sandbox   port: $port" -ForegroundColor Cyan

# moona routes flags differently per subcommand: the dashboard is `moona ui --port`,
# a session shortcut is `moona --port <cmd>`, and setup takes no port. Dispatch
# accordingly so --port always lands where the flag parser expects it.
switch ($sub) {
  ''       { & $exe ui --port $port }
  'ui'     { & $exe ui --port $port @tail }
  'setup'  { & $exe setup }
  'daemon' { & $exe daemon --port $port @tail }
  'ls'     { & $exe ls }
  'status' { & $exe status }
  'qr'     { & $exe qr }
  'url'    { & $exe url }
  default  { & $exe --port $port @Rest }   # e.g. `moona --port 8899 claude`
}
