# Builds the AirView Raspberry Pi SD-card images on Windows (no Linux needed).
#
#   powershell -File deploy\feeder\pi-image\build.ps1 -Base <raspios-lite-arm64.img.xz> -OutDir <dir> [-Version 1.2.3] [-Kinds station,feeder]
#
# -Base     the official Raspberry Pi OS Lite (64-bit) image (.img.xz or .img), verified against its
#           published SHA-256 by you (see README.md). Trixie (2025-10) or later.
# -OutDir   where the images go; binaries are built into <OutDir>\bin. Never the dist folder of a
#           running installation.
# -Version  stamped into the binaries (-X main.version) and the image names; default: model.Version.
# -Sha256   optional: the expected SHA-256 of -Base; the build stops when it differs.
param(
    [Parameter(Mandatory = $true)][string]$Base,
    [Parameter(Mandatory = $true)][string]$OutDir,
    [string]$Version = '',
    [string]$Kinds = 'station,feeder',
    [string]$Sha256 = '',
    # Base64 Ed25519 public key of the release signing key (go run ./deploy/release pubkey -key K) and the
    # release base URL (<URL>/feeder/manifest.json; default the aggregator): feeders from the image then
    # update themselves. Without -ManifestKey automatic updates are off (the setup page says so).
    [string]$ManifestKey = '',
    [string]$UpdateUrl = ''
)
$ErrorActionPreference = 'Stop'
$root = Resolve-Path (Join-Path $PSScriptRoot '..\..\..')
Set-Location $root

if (-not (Test-Path $Base)) { throw "base image not found: $Base" }
if ($Sha256) {
    $h = (Get-FileHash -Algorithm SHA256 $Base).Hash.ToLower()
    if ($h -ne $Sha256.ToLower()) { throw "SHA-256 of $Base is $h, expected $Sha256" }
    Write-Host "base SHA-256 verified: $h"
}
New-Item -ItemType Directory -Force $OutDir | Out-Null
$bin = Join-Path $OutDir 'bin'
New-Item -ItemType Directory -Force $bin | Out-Null

function Invoke-Go {
    param([string[]]$GoArgs)
    & go @GoArgs
    if ($LASTEXITCODE -ne 0) { throw "go $($GoArgs -join ' ') failed with exit code $LASTEXITCODE" }
}

$ld = '-s -w'
if ($Version) { $ld += " -X main.version=$Version" }
$env:CGO_ENABLED = '0'
try {
    $env:GOOS = 'linux'; $env:GOARCH = 'arm64'
    Write-Host '== linux/arm64 binaries' -ForegroundColor Cyan
    Invoke-Go @('build', '-trimpath', '-ldflags', $ld, '-o', (Join-Path $bin 'airview-linux-arm64'), './cmd/airview')
    Invoke-Go @('build', '-trimpath', '-ldflags', $ld, '-o', (Join-Path $bin 'airview-feeder-linux-arm64'), './cmd/airview-feeder')
    Invoke-Go @('build', '-trimpath', '-ldflags', '-s -w', '-o', (Join-Path $bin 'airview-pisetup'), './cmd/airview-piimage/pisetup')
} finally {
    Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
}
$tool = Join-Path $bin 'airview-piimage.exe'
Invoke-Go @('build', '-trimpath', '-o', $tool, './cmd/airview-piimage')

Write-Host '== images' -ForegroundColor Cyan
$a = @('build', '-base', $Base, '-out', $OutDir, '-kinds', $Kinds, '-repo', $root,
    '-airview', (Join-Path $bin 'airview-linux-arm64'),
    '-feeder', (Join-Path $bin 'airview-feeder-linux-arm64'),
    '-pisetup', (Join-Path $bin 'airview-pisetup'))
if ($Version) { $a += @('-version', $Version) }
if ($ManifestKey) { $a += @('-manifest-key', $ManifestKey) }
if ($UpdateUrl) { $a += @('-update-url', $UpdateUrl) }
& $tool @a
if ($LASTEXITCODE -ne 0) { throw "airview-piimage build failed ($LASTEXITCODE)" }
Write-Host "Images in $OutDir" -ForegroundColor Green
