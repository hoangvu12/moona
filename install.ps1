<#
.SYNOPSIS
    Installs (or upgrades) moona, a Windows-native web terminal.

.DESCRIPTION
    Downloads the latest moona release from GitHub, verifies its SHA256 against
    the release checksums.txt, extracts moona.exe into a per-user directory, and
    adds that directory to your user PATH. Re-running upgrades in place.

    The install location is user-writable on purpose, so `moona update` (and the
    automatic startup update) can replace the binary without administrator rights.

.EXAMPLE
    irm https://raw.githubusercontent.com/hoangvu12/moona/master/install.ps1 | iex

.EXAMPLE
    # Saved locally, pin a specific version:
    .\install.ps1 -Version v0.1.0
#>
[CmdletBinding()]
param(
    [string]$Version = "latest",
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA "Programs\moona")
)

$ErrorActionPreference = "Stop"
$Owner = "hoangvu12"
$Repo  = "moona"

function Write-Info($msg) { Write-Host "moona: $msg" -ForegroundColor Cyan }

# GitHub's API rejects requests without a User-Agent.
$headers = @{ "User-Agent" = "moona-installer"; "Accept" = "application/vnd.github+json" }

# --- architecture ---
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE (moona ships windows amd64 and arm64)" }
}

# --- resolve release ---
if ($Version -eq "latest") {
    $api = "https://api.github.com/repos/$Owner/$Repo/releases/latest"
} else {
    $api = "https://api.github.com/repos/$Owner/$Repo/releases/tags/$Version"
}
Write-Info "resolving release ($Version, $arch)"
try {
    $release = Invoke-RestMethod -Uri $api -Headers $headers
} catch {
    throw "Could not fetch release info from $api. Has a release been published yet? ($_)"
}
$tag = $release.tag_name

$assetName = "${Repo}_$($tag.TrimStart('v'))_windows_$arch.zip"
$asset = $release.assets | Where-Object { $_.name -eq $assetName } | Select-Object -First 1
if (-not $asset) {
    # Fallback: any windows/<arch> zip in this release.
    $asset = $release.assets | Where-Object { $_.name -like "*windows_$arch*.zip" } | Select-Object -First 1
}
if (-not $asset) { throw "No Windows $arch asset found in release $tag" }
$sums = $release.assets | Where-Object { $_.name -eq "checksums.txt" } | Select-Object -First 1

# --- download to a temp dir ---
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("moona_" + [System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp -Force | Out-Null
try {
    $zip = Join-Path $tmp $asset.name
    Write-Info "downloading $($asset.name)"
    Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $zip -Headers $headers

    # --- verify checksum ---
    if ($sums) {
        $sumsFile = Join-Path $tmp "checksums.txt"
        Invoke-WebRequest -Uri $sums.browser_download_url -OutFile $sumsFile -Headers $headers
        $line = Select-String -Path $sumsFile -Pattern ([regex]::Escape($asset.name)) | Select-Object -First 1
        if ($line) {
            $expected = ($line.Line -split '\s+')[0].ToLower()
            $actual = (Get-FileHash -Path $zip -Algorithm SHA256).Hash.ToLower()
            if ($expected -ne $actual) {
                throw "Checksum mismatch for $($asset.name)`n  expected $expected`n  actual   $actual"
            }
            Write-Info "checksum verified"
        } else {
            Write-Warning "no checksum entry for $($asset.name); skipping verification"
        }
    } else {
        Write-Warning "release has no checksums.txt; skipping verification"
    }

    # --- install ---
    # Stop a running moona so we can overwrite its exe.
    Get-Process -Name moona -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Write-Info "installing to $InstallDir"
    Expand-Archive -Path $zip -DestinationPath $InstallDir -Force
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$exe = Join-Path $InstallDir "moona.exe"
if (-not (Test-Path $exe)) { throw "moona.exe was not found in $InstallDir after extraction" }

# --- add to user PATH (idempotent) ---
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not $userPath) { $userPath = "" }
$parts = $userPath.Split(";", [System.StringSplitOptions]::RemoveEmptyEntries)
if ($parts -notcontains $InstallDir) {
    [Environment]::SetEnvironmentVariable("Path", (($parts + $InstallDir) -join ";"), "User")
    Write-Info "added $InstallDir to your user PATH (open a new terminal to pick it up)"
}
# Make it usable in this session too.
if (($env:Path -split ';') -notcontains $InstallDir) { $env:Path = "$env:Path;$InstallDir" }

Write-Host ""
Write-Info "installed $tag"
& $exe version
Write-Host ""
Write-Info "get started:  moona claude"
