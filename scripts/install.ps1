# Install the latest published bosun binary on Windows.
#
# Usage (PowerShell):
#   iwr -useb https://raw.githubusercontent.com/jasondillingham/bosun/main/scripts/install.ps1 | iex
#   $env:BOSUN_VERSION = 'v0.11.0'; iwr ... | iex                 # pin a version
#   $env:BOSUN_INSTALL_DIR = "$HOME\bin"; iwr ... | iex           # custom dest
#
# Resolves the latest release from GitHub (or honors $env:BOSUN_VERSION),
# downloads the matching ZIP, verifies its SHA-256 against the published
# checksums.txt, extracts bosun.exe, and drops it in $env:BOSUN_INSTALL_DIR
# (defaults to $HOME\bin, which is added to the user PATH if missing).

$ErrorActionPreference = 'Stop'

$Repo       = 'jasondillingham/bosun'
$Version    = $env:BOSUN_VERSION
$InstallDir = $env:BOSUN_INSTALL_DIR
if (-not $InstallDir) { $InstallDir = Join-Path $HOME 'bin' }

# Detect arch. PROCESSOR_ARCHITECTURE is the running shell's arch; under a
# 32-bit shell on a 64-bit OS, PROCESSOR_ARCHITEW6432 holds the OS arch.
$archRaw = $env:PROCESSOR_ARCHITEW6432
if (-not $archRaw) { $archRaw = $env:PROCESSOR_ARCHITECTURE }
switch ($archRaw) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { throw "Unsupported architecture: $archRaw" }
}

if (-not $Version) {
    # Resolve latest tag via the GitHub release-page redirect — no auth,
    # no rate-limited API call. The redirect points at /releases/tag/vX.Y.Z;
    # the last path segment is the tag.
    $resp = Invoke-WebRequest -UseBasicParsing -MaximumRedirection 0 -ErrorAction Ignore `
        -Uri "https://github.com/$Repo/releases/latest"
    $location = $resp.Headers.Location
    if (-not $location) { throw "Could not resolve latest version (no Location header)" }
    $Version = $location.Split('/')[-1]
}

# GoReleaser archive names drop the leading `v` from the version (see
# name_template in .goreleaser.yaml); the tag still has it.
$VersionNoV = $Version.TrimStart('v')
$Archive    = "bosun_${VersionNoV}_windows_${arch}.zip"
$Base       = "https://github.com/$Repo/releases/download/$Version"

$tmp = New-Item -ItemType Directory -Path (Join-Path $env:TEMP "bosun-install-$([guid]::NewGuid())")
try {
    Write-Host "Downloading $Archive ..."
    Invoke-WebRequest -UseBasicParsing -Uri "$Base/$Archive"      -OutFile (Join-Path $tmp $Archive)
    Invoke-WebRequest -UseBasicParsing -Uri "$Base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')

    # Verify SHA-256. checksums.txt is plain `<hex>  <filename>` lines.
    $expectedLine = Get-Content (Join-Path $tmp 'checksums.txt') |
        Where-Object { $_ -match "\s$([regex]::Escape($Archive))$" }
    if (-not $expectedLine) { throw "checksums.txt has no entry for $Archive" }
    $expected = ($expectedLine -split '\s+')[0]
    $actual   = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $Archive)).Hash.ToLower()
    if ($actual -ne $expected.ToLower()) {
        throw "Checksum mismatch for $Archive`n  expected: $expected`n  actual:   $actual"
    }

    Expand-Archive -Path (Join-Path $tmp $Archive) -DestinationPath $tmp -Force

    if (-not (Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir | Out-Null
    }
    Copy-Item -Force -Path (Join-Path $tmp 'bosun.exe') -Destination (Join-Path $InstallDir 'bosun.exe')

    Write-Host ""
    Write-Host "Installed bosun $Version -> $InstallDir\bosun.exe"

    # Add to the user PATH if missing. Machine PATH would need admin; user
    # PATH is sufficient and survives reboots.
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not ($userPath -split ';' | Where-Object { $_ -eq $InstallDir })) {
        [Environment]::SetEnvironmentVariable('Path', "$userPath;$InstallDir", 'User')
        Write-Host "Added $InstallDir to user PATH (restart your shell to pick it up)."
    }

    & (Join-Path $InstallDir 'bosun.exe') --version
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
