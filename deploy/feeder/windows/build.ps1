# AirView Feeder for Windows: binaries, portable ZIP and installer (WP-WF, DESIGN §12.3).
#
# Usage (from anywhere):
#   powershell -ExecutionPolicy Bypass -File deploy\feeder\windows\build.ps1
#              [-OutDir dir] [-Version x.y.z] [-Arch amd64,arm64] [-NoInstaller] [-Iscc path]
#              [-CertThumbprint sha1] [-SignTool path] [-TimestampUrl url]
#
# For every architecture it builds, into <OutDir>\stage-<arch>:
#   airview-feeder.exe        console program (commands, Windows service)
#   airview-feeder-tray.exe   the same package linked with -H windowsgui (no console window)
#   airview-feeder.ico        drawn by internal/wintray (deploy\feeder\windows\mkicon)
#   README.txt                portable\README.txt (ZIP) / installer\README.txt (installer)
# and then
#   AirView-Feeder-<version>-windows-<arch>.zip          portable ZIP (folder of the same name)
#   AirView-Feeder-Setup-<version>-windows-<arch>.exe    Inno Setup installer, when ISCC.exe is found
#
# Signing: with -CertThumbprint (a code-signing certificate in the user or machine store)
# the executables, the installer and its uninstaller are Authenticode-signed with
# signtool.exe (Windows SDK). Without it everything is unsigned (README.md: SmartScreen).
#
# Defaults: OutDir = deploy\feeder\windows\out (never the repository's dist folder);
# Version = the feeder's built-in version (internal/model.Version).
# Pure Go, CGO_ENABLED=0; downloads nothing.

[CmdletBinding()]
param(
    [string]$OutDir,
    [string]$Version,
    [string[]]$Arch = @('amd64', 'arm64'),
    [switch]$NoInstaller,
    [string]$Iscc,
    [string]$CertThumbprint,
    [string]$SignTool,
    [string]$TimestampUrl = 'http://timestamp.digicert.com',
    # ReleaseStage: called by deploy\build.ps1 (the Release stage), which may write below dist.
    [switch]$ReleaseStage
)

$ErrorActionPreference = 'Stop'
$here = $PSScriptRoot
$root = (Resolve-Path (Join-Path $here '..\..\..')).Path
if (-not $OutDir) { $OutDir = Join-Path $here 'out' }
New-Item -ItemType Directory -Force $OutDir | Out-Null
$OutDir = (Resolve-Path $OutDir).Path
if (-not $ReleaseStage -and ($OutDir.TrimEnd('\') -ieq (Join-Path $root 'dist') -or $OutDir -ilike ((Join-Path $root 'dist') + '\*'))) {
    throw "Refusing to write into $root\dist (the Release stage owns it). Use -OutDir."
}

function Invoke-Go {
    param([string[]]$GoArgs, [hashtable]$Env = @{})
    $saved = @{}
    foreach ($k in $Env.Keys) { $saved[$k] = [Environment]::GetEnvironmentVariable($k); [Environment]::SetEnvironmentVariable($k, $Env[$k]) }
    try {
        Push-Location $root
        & go @GoArgs
        if ($LASTEXITCODE -ne 0) { throw "go $($GoArgs -join ' ') failed ($LASTEXITCODE)" }
    } finally {
        Pop-Location
        foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
    }
}

function Find-Iscc {
    if ($Iscc) { if (Test-Path $Iscc) { return (Resolve-Path $Iscc).Path } else { throw "ISCC not found at $Iscc" } }
    $cmd = Get-Command 'ISCC.exe' -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    foreach ($base in @(${env:ProgramFiles(x86)}, $env:ProgramFiles, (Join-Path $env:LOCALAPPDATA 'Programs'))) {
        if (-not $base) { continue }
        $p = Join-Path $base 'Inno Setup 6\ISCC.exe'
        if (Test-Path $p) { return $p }
    }
    return $null
}

function Find-SignTool {
    if ($SignTool) { return $SignTool }
    $cmd = Get-Command 'signtool.exe' -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    $kits = Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10\bin'
    if (Test-Path $kits) {
        $c = Get-ChildItem $kits -Recurse -Filter signtool.exe -ErrorAction SilentlyContinue |
            Where-Object { $_.FullName -match '\\x64\\' } | Sort-Object FullName -Descending | Select-Object -First 1
        if ($c) { return $c.FullName }
    }
    throw 'signtool.exe not found (install the Windows SDK or pass -SignTool).'
}

function Sign-Files([string[]]$Files) {
    if (-not $CertThumbprint) { return }
    $st = Find-SignTool
    & $st sign /fd sha256 /sha1 $CertThumbprint /tr $TimestampUrl /td sha256 @Files
    if ($LASTEXITCODE -ne 0) { throw "signtool failed ($LASTEXITCODE)" }
}

# New-Zip writes every file under $Dir into $Zip with forward-slash entry names
# (Compress-Archive of Windows PowerShell 5.1 writes backslashes, which other
# unzip programs treat as part of the file name).
function New-Zip([string]$Dir, [string]$Zip) {
    Add-Type -AssemblyName System.IO.Compression, System.IO.Compression.FileSystem
    $base = (Resolve-Path $Dir).Path.TrimEnd('\') + '\'
    $fs = [IO.File]::Open($Zip, 'CreateNew')
    try {
        $za = New-Object IO.Compression.ZipArchive($fs, [IO.Compression.ZipArchiveMode]::Create)
        try {
            foreach ($f in Get-ChildItem $Dir -Recurse -File | Sort-Object FullName) {
                $name = $f.FullName.Substring($base.Length).Replace('\', '/')
                [void][IO.Compression.ZipFileExtensions]::CreateEntryFromFile($za, $f.FullName, $name, [IO.Compression.CompressionLevel]::Optimal)
            }
        } finally { $za.Dispose() }
    } finally { $fs.Dispose() }
}
$hostExe = Join-Path $OutDir 'host-airview-feeder.exe'
$ldVersion = if ($Version) { " -X main.version=$Version" } else { '' }
if (-not $Version) {
    Invoke-Go @('build', '-o', $hostExe, './cmd/airview-feeder') @{ CGO_ENABLED = '0'; GOOS = ''; GOARCH = '' }
    $Version = ((& $hostExe version) -split '\s+')[-1].Trim()
    Remove-Item $hostExe -Force
    if (-not $Version) { throw 'could not read the feeder version' }
}
Write-Host "AirView Feeder $Version -> $OutDir" -ForegroundColor Cyan

$icon = Join-Path $OutDir 'airview-feeder.ico'
Invoke-Go @('run', './deploy/feeder/windows/mkicon', '-o', $icon) @{ CGO_ENABLED = '0'; GOOS = ''; GOARCH = '' }

$isccPath = $null
if (-not $NoInstaller) {
    $isccPath = Find-Iscc
    if (-not $isccPath) {
        Write-Host 'Inno Setup 6 (ISCC.exe) not found: the installer is skipped; the portable ZIP is built. Install Inno Setup 6.3+ or pass -Iscc.' -ForegroundColor Yellow
    }
}

foreach ($a in $Arch) {
    if ($a -notin @('amd64', 'arm64')) { throw "unsupported -Arch $a (amd64 or arm64)" }
    Write-Host "== windows/$a ==" -ForegroundColor Cyan
    $stage = Join-Path $OutDir "stage-$a"
    if (Test-Path $stage) { Remove-Item $stage -Recurse -Force }
    New-Item -ItemType Directory -Force $stage | Out-Null
    $envs = @{ CGO_ENABLED = '0'; GOOS = 'windows'; GOARCH = $a }
    Invoke-Go @('build', '-trimpath', '-ldflags', "-s -w$ldVersion", '-o', (Join-Path $stage 'airview-feeder.exe'), './cmd/airview-feeder') $envs
    Invoke-Go @('build', '-trimpath', '-ldflags', "-H windowsgui -s -w$ldVersion", '-o', (Join-Path $stage 'airview-feeder-tray.exe'), './cmd/airview-feeder') $envs
    Sign-Files @((Join-Path $stage 'airview-feeder.exe'), (Join-Path $stage 'airview-feeder-tray.exe'))
    Copy-Item $icon $stage
    # Licence terms and the third-party notices of the linked modules (the Go standard library's BSD
    # licence must travel with the binaries; DLV-05). Both go into the ZIP and the installer.
    $licText = [IO.File]::ReadAllText((Join-Path $root 'deploy\installer\LICENSE.txt')) -replace "`r?`n", "`r`n"
    [IO.File]::WriteAllText((Join-Path $stage 'LICENSE.txt'), $licText, (New-Object Text.UTF8Encoding $false))
    Invoke-Go @('run', './deploy/relkit', 'notices', '-pkg', './cmd/airview-feeder', '-goos', 'windows', '-goarch', $a, '-crlf',
        '-o', (Join-Path $stage 'THIRD-PARTY-NOTICES.txt')) @{ CGO_ENABLED = '0'; GOOS = ''; GOARCH = '' }

    # Portable ZIP: one top-level folder.
    $name = "AirView-Feeder-$Version-windows-$a"
    $zipRoot = Join-Path $OutDir "zip-$a"
    $folder = Join-Path $zipRoot $name
    if (Test-Path $zipRoot) { Remove-Item $zipRoot -Recurse -Force }
    New-Item -ItemType Directory -Force $folder | Out-Null
    Copy-Item (Join-Path $stage '*.exe'), (Join-Path $stage '*.ico'), (Join-Path $stage 'LICENSE.txt'), (Join-Path $stage 'THIRD-PARTY-NOTICES.txt') $folder
    Copy-Item (Join-Path $here 'portable\*') $folder
    $zip = Join-Path $OutDir "$name.zip"
    if (Test-Path $zip) { Remove-Item $zip -Force }
    New-Zip $zipRoot $zip
    Remove-Item $zipRoot -Recurse -Force
    Write-Host "  $zip"

    # Installer.
    Copy-Item (Join-Path $here 'installer\README.txt') (Join-Path $stage 'README.txt') -Force
    if ($isccPath) {
        $isccArgs = @("/DArch=$a", "/DAppVersion=$Version", "/DSrcDir=$stage", "/O$OutDir", '/Q')
        if ($CertThumbprint) {
            $st = Find-SignTool
            $isccArgs += @('/DSign=1', "/Ssigntool=`"$st`" sign /fd sha256 /sha1 $CertThumbprint /tr $TimestampUrl /td sha256 `$f")
        }
        & $isccPath @isccArgs (Join-Path $here 'airview-feeder.iss')
        if ($LASTEXITCODE -ne 0) { throw "ISCC failed ($LASTEXITCODE)" }
        Write-Host "  $(Join-Path $OutDir "AirView-Feeder-Setup-$Version-windows-$a.exe")"
    }
}

Get-ChildItem $OutDir -File | Where-Object { $_.Extension -in '.zip', '.exe' } |
    ForEach-Object { '{0,-60} {1,8:N1} MB  sha256 {2}' -f $_.Name, ($_.Length / 1MB), (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower() }
