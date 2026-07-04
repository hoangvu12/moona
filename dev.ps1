# dev.ps1 - hot-reload dev loop for moona.
#
#   .\dev.ps1          LOCAL desktop loop  -> open http://127.0.0.1:8899/
#   .\dev.ps1 phone    PHONE loop          -> also opens a Cloudflare tunnel and
#                                             prints a tokened public URL you can
#                                             open on your phone.
#
# Both modes: edit any .go file -> Air rebuilds ./cmd/moona and restarts the
# SANDBOX daemon (:8899); edit cmd/moona/web/index.html -> the browser auto-reloads
# with NO rebuild (MOONA_DEV serves the page from disk + pushes a reload frame over
# the terminal WebSocket). Everything runs in the isolated .testhome sandbox, so it
# NEVER touches the real :8787 daemon that hosts your Claude Code session.
# Ctrl+C stops the loop. Delete .testhome to reset.
#
# One-time setup:  go install github.com/air-verse/air@latest   (Go bin dir on PATH)

param([Parameter(ValueFromRemainingArguments = $true)] $Rest)
$ErrorActionPreference = 'Stop'
$root    = $PSScriptRoot
$sandbox = Join-Path $root '.testhome'
$port    = 8899
$mode    = if ($Rest.Count -gt 0) { $Rest[0] } else { 'local' }

# Capture the REAL LocalAppData before we override it, so we can find the
# cloudflared.exe that `moona setup` already downloaded.
$realLocal = [Environment]::GetFolderPath('LocalApplicationData')

New-Item -ItemType Directory -Force -Path $sandbox | Out-Null

# --- shared sandbox + dev env (propagates to the Air-launched daemon) ---
$env:LOCALAPPDATA    = $sandbox
$env:MOONA_NO_UPDATE = '1'
$env:MOONA_DEV       = '1'
$env:MOONA_DEV_WEB   = Join-Path $root 'cmd\moona\web\index.html'
$env:MOONA_DEV_SEED  = 'powershell.exe -NoLogo'   # a tab is always live after a rebuild

$air = Get-Command air -ErrorAction SilentlyContinue
if (-not $air) {
  Write-Host "air not found on PATH." -ForegroundColor Yellow
  Write-Host "Install once:  go install github.com/air-verse/air@latest" -ForegroundColor Yellow
  Write-Host "then ensure your Go bin dir (e.g. `$env:USERPROFILE\go\bin) is on PATH." -ForegroundColor Yellow
  exit 1
}

# Fixed dev hostname, served by a dedicated named Cloudflare tunnel (created once
# with:  cloudflared tunnel create moona-dev
#        cloudflared tunnel route dns moona-dev moona-dev.nguyenvu.dev ).
# A NAMED tunnel keeps the same URL across daemon rebuilds AND tunnel restarts.
$devTunnel = 'moona-dev'
$devHost   = 'moona-dev.nguyenvu.dev'

if ($mode -eq 'phone') {
  # Stable dev token, persisted so the phone's saved token survives restarts.
  $tokenFile = Join-Path $sandbox 'dev-token.txt'
  if (Test-Path $tokenFile) { $token = (Get-Content $tokenFile -Raw).Trim() }
  else { $token = 'dev' + [guid]::NewGuid().ToString('N').Substring(0, 12); Set-Content $tokenFile $token -NoNewline }
  $env:MOONA_TOKEN = $token   # the Air-launched daemon reads this (no --token flag needed)

  # Find cloudflared (the copy moona already downloaded, or one on PATH).
  $cf = (Get-Command cloudflared -ErrorAction SilentlyContinue).Source
  if (-not $cf) {
    $cand = Join-Path $realLocal 'moona\cloudflared.exe'
    if (Test-Path $cand) { $cf = $cand }
  }
  if (-not $cf) {
    Write-Host "cloudflared not found. Run 'moona setup' once so the tunnel helper is downloaded." -ForegroundColor Yellow
    exit 1
  }

  # A standalone named tunnel to :8899 that OUTLIVES Air's daemon restarts, so the
  # phone URL (https://$devHost) stays constant as the daemon rebuilds underneath.
  $cfLog = Join-Path $sandbox 'cf-named.log'
  Remove-Item $cfLog -ErrorAction SilentlyContinue
  $cfProc = Start-Process -FilePath $cf `
    -ArgumentList @('tunnel', '--url', "http://127.0.0.1:$port", 'run', $devTunnel) `
    -RedirectStandardError $cfLog -RedirectStandardOutput (Join-Path $sandbox 'cf.out') `
    -WindowStyle Hidden -PassThru
  Write-Host "phone dev: connecting named tunnel '$devTunnel' -> $devHost ..." -ForegroundColor Cyan

  # Wait for the tunnel to register an edge connection (log line) before declaring
  # the fixed URL ready.
  $ready = $false
  for ($i = 0; $i -lt 60 -and -not $ready; $i++) {
    Start-Sleep -Milliseconds 500
    if ((Test-Path $cfLog) -and (Select-String -Path $cfLog -Pattern 'Registered tunnel connection' -Quiet -ErrorAction SilentlyContinue)) {
      $ready = $true
    }
  }
  $url = "https://$devHost/?token=$token"
  Set-Content (Join-Path $sandbox 'dev-url.txt') $url -NoNewline
  Write-Host ""
  if ($ready) { Write-Host "  Phone dev URL:  $url" -ForegroundColor Green }
  else { Write-Host "  Phone dev URL:  $url  (tunnel still connecting - check $cfLog)" -ForegroundColor Yellow }
  Write-Host "  (open on your phone; the token is saved after the first visit)" -ForegroundColor DarkGray
  Write-Host ""

  # Make sure the tunnel dies with this script.
  $exitScript = { if ($cfProc -and -not $cfProc.HasExited) { try { $cfProc.Kill() } catch {} } }
  Register-EngineEvent PowerShell.Exiting -Action $exitScript | Out-Null
}

Write-Host "moona dev loop [$mode]  |  sandbox http://127.0.0.1:$port/  |  UI hot-reload ON" -ForegroundColor Cyan
Write-Host "sandbox: $sandbox   (delete to reset)" -ForegroundColor DarkGray
& air
