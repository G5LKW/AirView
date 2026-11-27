# AirView Windows installers (WP-SETUP-exe): our own pure-Go installer, no Inno Setup, nothing downloaded.
#
# Usage (Windows PowerShell 5.1+, from anywhere):
#   powershell -ExecutionPolicy Bypass -File deploy\installer\build-installers.ps1
#              [-OutDir dir] [-Version x.y.z] [-Arch amd64,arm64] [-StationArch amd64]
#              [-BinDir dir] [-SkipStation] [-SkipFeeder] [-UserStub] [-Resources] [-ReleaseStage]
#
# Produces in -OutDir (default deploy\installer\out, never dist\ unless -ReleaseStage):
#   AirView-Setup-<ver>.exe                  station installer (windows/amd64)
#   AirView-Feeder-Setup-<ver>.exe           feeder installer (windows/amd64)
#   AirView-Feeder-Setup-<ver>-arm64.exe     feeder installer (windows/arm64)
#   (other -StationArch values: AirView-Setup-<ver>-<arch>.exe)
#
# Each installer is cmd\airview-setup (the stub: wizard, silent mode, uninstaller) followed by a payload
# assembled by cmd\airview-setup\mkinstaller from deploy\installer\products\<product>.json and:
#   station: airview.exe, README.txt, LICENSE.txt, THIRD-PARTY-NOTICES.txt, docs\*.md
#   feeder:  airview-feeder.exe, airview-feeder-tray.exe, airview-feeder.ico, README.txt, LICENSE.txt,
#            THIRD-PARTY-NOTICES.txt, docs\*.md
# THIRD-PARTY-NOTICES.txt is generated from the licence files of the Go modules linked into the program
# (go list -deps, read from the local module cache) plus, for the station, web\static\vendor.
#
# -BinDir      use prebuilt binaries instead of building them: airview.exe (station, amd64; or
#              airview-windows-<arch>.exe) and airview-feeder-windows-<arch>.exe. The Release stage passes dist\.
# -AdminStub   link the requireAdministrator stub (go build -tags setupadmin): UAC prompt at every start, so
#              /CURRENTUSER also needs an administrator. The default stub is as-invoker (DLV-06): a per-user
#              install (/CURRENTUSER) runs without UAC, a per-machine install (the default) asks for elevation
#              itself (UAC prompt, also with /S).
# -UserStub    accepted for compatibility; the as-invoker stub is the default.
# -ManifestKey base64 Ed25519 public key of the release signing key (deploy\release pubkey), and
# -UpdateUrl   the feeder's signed release manifest URL (https://.../feeder/manifest.json): baked into the feeder
#              installers (mkinstaller -var manifestkey= / -var updateurl=), which write update.keys and
#              update.manifestUrl into feeder.json so installed feeders update themselves (DLV-03). Without
#              -ManifestKey the feeder's automatic updates stay unconfigured.
# -Resources   regenerate cmd\airview-setup\res\{admin,user}\rsrc_windows_*.syso from winres.json with
#              go-winres (as deploy\build.ps1 -Resources does for the other programs; needs the module cache or
#              network access the first time). The .syso files are checked in.
# Installers are unsigned (DESIGN section 19, decision 5); see deploy\installer\README.md for SmartScreen.
[CmdletBinding()]
param(
    [string]$OutDir,
    [string]$Version,
    [string[]]$Arch = @('amd64', 'arm64'),
    [string[]]$StationArch = @('amd64'),
    [string]$BinDir,
    [switch]$SkipStation,
    [switch]$SkipFeeder,
    [switch]$UserStub,
    [switch]$AdminStub,
    [string]$ManifestKey,
    [string]$UpdateUrl,
    [switch]$Resources,
    [switch]$ReleaseStage
)

$ErrorActionPreference = 'Stop'
$here = $PSScriptRoot
$root = (Resolve-Path (Join-Path $here '..\..')).Path
if (-not $OutDir) { $OutDir = Join-Path $here 'out' }
New-Item -ItemType Directory -Force $OutDir | Out-Null
$OutDir = (Resolve-Path $OutDir).Path
$distDir = Join-Path $root 'dist'
if (-not $ReleaseStage -and ($OutDir.TrimEnd('\') -ieq $distDir -or $OutDir -ilike ($distDir + '\*'))) {
    throw "Refusing to write into $distDir (the Release stage owns it). Use -OutDir."
}

function Invoke-Go {
    param([string[]]$GoArgs, [hashtable]$Env = @{})
    $saved = @{}
    foreach ($k in $Env.Keys) { $saved[$k] = [Environment]::GetEnvironmentVariable($k); [Environment]::SetEnvironmentVariable($k, $Env[$k]) }
    try {
        Push-Location $root
        $out = & go @GoArgs
        if ($LASTEXITCODE -ne 0) { throw "go $($GoArgs -join ' ') failed ($LASTEXITCODE)" }
        return $out
    } finally {
        Pop-Location
        foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
    }
}

$hostEnv = @{ CGO_ENABLED = '0'; GOOS = ''; GOARCH = ''; GOARM = '' }

if (-not $Version) {
    $m = Select-String -Path (Join-Path $root 'internal\model\model.go') -Pattern '^const Version = "([^"]+)"'
    if (-not $m) { throw 'cannot read the version from internal\model\model.go; pass -Version' }
    $Version = $m.Matches[0].Groups[1].Value
}
$ldVersion = if ($PSBoundParameters.ContainsKey('Version')) { " -X main.version=$Version" } else { '' }
Write-Host "AirView installers $Version -> $OutDir" -ForegroundColor Cyan

if ($Resources) {
    Write-Host '== setup resources (go-winres) ==' -ForegroundColor Cyan
    foreach ($v in 'admin', 'user') {
        Push-Location (Join-Path $root "cmd\airview-setup\res\$v")
        try {
            & go run github.com/tc-hib/go-winres@v0.3.3 make --in winres.json --out rsrc --arch amd64,arm64
            if ($LASTEXITCODE -ne 0) { throw 'go-winres failed' }
        } finally { Pop-Location }
    }
}

if ($ManifestKey -and $ManifestKey -notmatch '^[A-Za-z0-9+/]{43}=$') { throw "-ManifestKey '$ManifestKey' is not the base64 of a 32-byte Ed25519 key" }
if ($UpdateUrl -and $UpdateUrl -notmatch '^https://[^\s"]+$') { throw "-UpdateUrl must be an https:// URL" }
if ($UpdateUrl -and -not $ManifestKey) { throw '-UpdateUrl needs -ManifestKey (unsigned updates are never configured)' }
$updateVars = @()
if ($ManifestKey) {
    $updateVars = @('-var', "manifestkey=$ManifestKey", '-var', "updateurl=$UpdateUrl")
    Write-Host "feeder installers: update.keys=$ManifestKey$(if ($UpdateUrl) { ", update.manifestUrl=$UpdateUrl" })"
} elseif (-not $SkipFeeder) {
    Write-Host 'no -ManifestKey: installed feeders will not update automatically until update.keys / update.manifestUrl are set' -ForegroundColor Yellow
}

$work = Join-Path $OutDir 'work'
if (Test-Path $work) { Remove-Item -Recurse -Force $work }
New-Item -ItemType Directory -Force $work | Out-Null

# The installer assembler runs on the build machine.
$mk = Join-Path $work 'mkinstaller.exe'
Invoke-Go @('build', '-o', $mk, './cmd/airview-setup/mkinstaller') $hostEnv | Out-Null

# New-Notices writes THIRD-PARTY-NOTICES.txt for the modules linked into $Pkg (GOOS=windows/$A).
function New-Notices {
    param([string]$Pkg, [string]$A, [string]$Out, [switch]$Web)
    $env1 = @{ CGO_ENABLED = '0'; GOOS = 'windows'; GOARCH = $A; GOARM = '' }
    $lines = Invoke-Go @('list', '-deps', '-f', '{{with .Module}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}', $Pkg) $env1
    $mods = $lines | Where-Object { $_ -and $_ -notlike 'airview|*' } | Sort-Object -Unique
    $sb = New-Object Text.StringBuilder
    [void]$sb.AppendLine('Third-party notices')
    [void]$sb.AppendLine('===================')
    [void]$sb.AppendLine('')
    [void]$sb.AppendLine('This program includes the following open-source components. Their licences follow.')
    [void]$sb.AppendLine('')
    $goroot = (Invoke-Go @('env', 'GOROOT') $hostEnv | Select-Object -First 1).Trim()
    $entries = @(@{ Name = 'The Go programming language (standard library and runtime)'; File = (Join-Path $goroot 'LICENSE') })
    foreach ($m in $mods) {
        $p = $m.Split('|')
        $lic = Get-ChildItem -LiteralPath $p[2] -File -ErrorAction SilentlyContinue | Where-Object { $_.Name -match '^(LICEN[CS]E|COPYING)(\.(md|txt))?$' } | Select-Object -First 1
        if (-not $lic) { throw "no licence file for module $($p[0]) in $($p[2])" }
        $entries += @{ Name = "$($p[0]) $($p[1])"; File = $lic.FullName }
    }
    if ($Web) {
        foreach ($f in Get-ChildItem (Join-Path $root 'web\static\vendor') -Recurse -File | Where-Object { $_.Name -like 'LICENSE*' } | Sort-Object FullName) {
            $entries += @{ Name = 'web/static/vendor/' + $f.Directory.Name + ' (' + $f.Name + ')'; File = $f.FullName }
        }
    }
    foreach ($e in $entries) {
        [void]$sb.AppendLine(('-' * 78))
        [void]$sb.AppendLine($e.Name)
        [void]$sb.AppendLine(('-' * 78))
        [void]$sb.AppendLine(([IO.File]::ReadAllText($e.File) -replace "`r?`n", "`r`n").TrimEnd())
        [void]$sb.AppendLine('')
    }
    [IO.File]::WriteAllText($Out, $sb.ToString(), (New-Object Text.UTF8Encoding $false))
}

function Build-Stub([string]$A) {
    $stub = Join-Path $work "airview-setup-$A.exe"
    $args1 = @('build', '-trimpath', '-ldflags', '-H windowsgui -s -w', '-o', $stub)
    if ($AdminStub) { $args1 += @('-tags', 'setupadmin') }
    Invoke-Go ($args1 + @('./cmd/airview-setup')) @{ CGO_ENABLED = '0'; GOOS = 'windows'; GOARCH = $A; GOARM = '' } | Out-Null
    return $stub
}

function Get-Binary([string]$Name, [string]$Prebuilt, [string]$Pkg, [string]$A, [string]$LdFlags) {
    if ($BinDir) {
        $p = Join-Path $BinDir $Prebuilt
        if (-not (Test-Path $p)) { throw "-BinDir: $p not found" }
        return (Resolve-Path $p).Path
    }
    $out = Join-Path $work "$A\$Name"
    New-Item -ItemType Directory -Force (Split-Path $out) | Out-Null
    Invoke-Go @('build', '-trimpath', '-ldflags', ($LdFlags + $ldVersion), '-o', $out, $Pkg) @{ CGO_ENABLED = '0'; GOOS = 'windows'; GOARCH = $A; GOARM = '' } | Out-Null
    return $out
}

function Invoke-Mk([string[]]$MkArgs) {
    & $mk @MkArgs
    if ($LASTEXITCODE -ne 0) { throw "mkinstaller failed ($LASTEXITCODE)" }
}

$docs = Join-Path $root 'docs\aggregator'
$licence = Join-Path $here 'LICENSE.txt'
$built = @()

if (-not $SkipStation) {
    foreach ($a in $StationArch) {
        Write-Host "== station installer windows/$a ==" -ForegroundColor Cyan
        $stub = Build-Stub $a
        $prebuilt = if ($a -eq 'amd64') { 'airview.exe' } else { "airview-windows-$a.exe" }
        $exe = Get-Binary 'airview.exe' $prebuilt './cmd/airview' $a '-H windowsgui -s -w'
        $notices = Join-Path $work "station-notices-$a.txt"
        New-Notices './cmd/airview' $a $notices -Web
        $name = if ($a -eq 'amd64') { "AirView-Setup-$Version.exe" } else { "AirView-Setup-$Version-$a.exe" }
        $out = Join-Path $OutDir $name
        Invoke-Mk @('-stub', $stub, '-product', (Join-Path $here 'products\station.json'), '-version', $Version, '-out', $out,
            '-file', "airview.exe=$exe",
            '-file', "README.txt=$(Join-Path $here 'README-station.txt')",
            '-file', "LICENSE.txt=$licence",
            '-file', "THIRD-PARTY-NOTICES.txt=$notices",
            '-file', "docs/OPERATOR-GUIDE.md=$(Join-Path $docs 'OPERATOR-GUIDE.md')",
            '-file', "docs/FEEDER-GUIDE.md=$(Join-Path $docs 'FEEDER-GUIDE.md')",
            '-file', "docs/PRIVACY-NOTICE.md=$(Join-Path $docs 'PRIVACY-NOTICE.md')",
            '-file', "docs/README.md=$(Join-Path $root 'README.md')")
        $built += $out
    }
}

if (-not $SkipFeeder) {
    $icon = Join-Path $work 'airview-feeder.ico'
    Invoke-Go @('run', './deploy/feeder/windows/mkicon', '-o', $icon) $hostEnv | Out-Null
    foreach ($a in $Arch) {
        if ($a -notin @('amd64', 'arm64')) { throw "unsupported -Arch $a (amd64 or arm64)" }
        Write-Host "== feeder installer windows/$a ==" -ForegroundColor Cyan
        $stub = Build-Stub $a
        $feeder = Get-Binary 'airview-feeder.exe' "airview-feeder-windows-$a.exe" './cmd/airview-feeder' $a '-s -w'
        # The tray copy is the same package linked for the GUI subsystem (no console window); always built.
        $tray = Join-Path $work "$a\airview-feeder-tray.exe"
        New-Item -ItemType Directory -Force (Split-Path $tray) | Out-Null
        Invoke-Go @('build', '-trimpath', '-ldflags', ('-H windowsgui -s -w' + $ldVersion), '-o', $tray, './cmd/airview-feeder') @{ CGO_ENABLED = '0'; GOOS = 'windows'; GOARCH = $a; GOARM = '' } | Out-Null
        $notices = Join-Path $work "feeder-notices-$a.txt"
        New-Notices './cmd/airview-feeder' $a $notices
        $name = if ($a -eq 'amd64') { "AirView-Feeder-Setup-$Version.exe" } else { "AirView-Feeder-Setup-$Version-$a.exe" }
        $out = Join-Path $OutDir $name
        Invoke-Mk (@('-stub', $stub, '-product', (Join-Path $here 'products\feeder.json'), '-version', $Version, '-out', $out,
            '-file', "airview-feeder.exe=$feeder",
            '-file', "airview-feeder-tray.exe=$tray",
            '-file', "airview-feeder.ico=$icon",
            '-file', "README.txt=$(Join-Path $root 'deploy\feeder\windows\installer\README.txt')",
            '-file', "LICENSE.txt=$licence",
            '-file', "THIRD-PARTY-NOTICES.txt=$notices",
            '-file', "docs/FEEDER-GUIDE.md=$(Join-Path $docs 'FEEDER-GUIDE.md')",
            '-file', "docs/PRIVACY-NOTICE.md=$(Join-Path $docs 'PRIVACY-NOTICE.md')" ) + $updateVars)
        $built += $out
    }
}

Remove-Item -Recurse -Force $work
foreach ($f in $built) {
    '{0,-46} {1,8:N1} MB  sha256 {2}' -f (Split-Path -Leaf $f), ((Get-Item $f).Length / 1MB), (Get-FileHash $f -Algorithm SHA256).Hash.ToLower()
}
