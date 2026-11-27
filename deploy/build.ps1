# AirView release build (Windows PowerShell 5.1+). One run produces every deliverable.
#
# Usage:  powershell -ExecutionPolicy Bypass -File deploy\build.ps1 [-OutDir dir] [-Version x.y.z]
#             [-SkipTests] [-WindowsOnly] [-SkipFeeder] [-SkipInstallers] [-SkipDocker] [-Resources]
#             [-PiImages raspios-lite-arm64.img.xz [-PiBaseSha256 <published hash>] [-PiImageKinds station,feeder]
#              [-KeepRawImages]]
#             [-SignKey C:\keys\airview-release.key [-Channel stable|beta] [-ManifestExpiresDays N]]
#
# Output (in -OutDir, default <repo>\dist; use a scratch directory for test builds so the dist\ of a running
# installation is not touched). <v> is -Version, default internal/model.Version.
#
#   Windows
#     airview.exe                                   desktop app (WebView2 window + embedded server), amd64
#     AirView-Setup-<v>.exe                         station installer (pure Go, deploy\installer; unsigned)
#     AirView-Feeder-Setup-<v>.exe / -arm64.exe     feeder installers (amd64 / Windows on Arm)
#     AirView-Feeder-<v>-windows-{amd64,arm64}.zip  portable feeder
#     airview-feeder-windows-{amd64,arm64}.exe      feeder binaries (also the self-update artifacts)
#   Raspberry Pi
#     airview-pi-<v>.tar.gz                         station app: arm64 + armv7 binaries, install.sh (idempotent,
#                                                   versioned, roll-back), update.sh, uninstall.sh, unit, README
#     airview-pi-install.sh                         its one-line installer (curl ... | sudo sh -s -- --base-url URL)
#     airview-feeder-pi-<v>.tar.gz                  feeder: arm64/armv7/amd64 binaries + deploy\feeder\pi
#     airview-feeder-install.sh                     the feeder's one-line installer (also served by aggregators)
#     AirView-{Station,Feeder}-Pi-<v>.img.zip       SD-card images (+ .sha256, .verify.txt), only with -PiImages
#   Linux servers (VPS)
#     airview-server-linux-{amd64,arm64}.tar.gz     server kit (deploy\server): binary, install/upgrade/uninstall/
#                                                   backup/restore, unit, logrotate, proxy examples
#     airview-server-install.sh                     one-line server installer (--base-url URL)
#     airview-linux-{arm64,armv7,amd64}             headless binaries (self-update artifacts)
#     airview-feeder-linux-{arm64,armv7,amd64}      headless feeder binaries (self-update artifacts)
#   Docker
#     airview-docker-<v>.tar.gz                     build context from the prebuilt binaries (deploy\docker)
#   Integrity
#     SHA256SUMS                                    sha256sum -c compatible, every artifact of this build
#     release\feeder\, release\airview\             signed update manifests + artifacts, only with -SignKey
#
# Every Linux bundle is packed by deploy\relkit (root-owned, 0755 for ELF binaries and #! scripts, 0644 for the
# rest, reproducible with SOURCE_DATE_EPOCH) and every artifact is verified at the end (relkit check: tar
# modes and owners, static ELF for the right CPU, PE machine, ZIP CRCs, VERSION files, SHA256SUMS). The run
# fails when anything is wrong.
#
# -SkipTests       skip go vet / go test ./...
# -WindowsOnly     only the Windows artifacts.
# -SkipFeeder      no feeder artifacts.
# -SkipInstallers  no Windows installers (deploy\installer\build-installers.ps1).
# -SkipDocker      no airview-docker-<v>.tar.gz.
# -Resources       regenerate the Windows resources (*.syso) with go-winres (network access the first time).
# -PiImages        path of the official Raspberry Pi OS Lite (64-bit) image (.img.xz) that YOU downloaded and
#                  verified; with it the SD-card images are built (cmd\airview-piimage). -PiBaseSha256 is its
#                  published SHA-256 (the build stops when it differs). -PiBase is an alias.
# -KeepRawImages   also ship the uncompressed .img files (large; the .img.zip is enough).
# -SignKey         Ed25519 release key (PEM, from "go run ./deploy/release keygen"). It must live outside the
#                  repository; the build refuses a key inside it. With it the update manifests of airview and
#                  airview-feeder are published, signed and verified into <OutDir>\release\. Its public key is
#                  also baked into the feeder installers and the feeder Pi image (update.keys), so installed
#                  feeders update themselves (DLV-03). The real release key is generated once
#                  (go run ./deploy/release keygen -o <offline path>), kept offline with a backup, and never in a
#                  temporary folder: the build refuses a key under %TEMP% / %TMP% or named *test* unless
#                  -AllowTestKey (test builds only; DLV-04).
# -UpdateBaseUrl   https base URL where <OutDir>\release\ is published (e.g. https://releases.example.net/airview):
#                  the Windows feeder installers get update.manifestUrl=<base>/feeder/manifest.json and the
#                  feeder image --url <base>. Without it, Pi feeders fetch updates from their aggregator
#                  (https://<aggregator>/feeder/manifest.json) and Windows feeders get only the key.
#
# Sources edited while the build runs make the artifacts inconsistent: the build records its start time and
# fails at the end when any source file (cmd, internal, web, deploy, go.mod/go.sum) changed meanwhile (DLV-01).
param(
    [string]$OutDir,
    [string]$Version,
    [switch]$SkipTests,
    [switch]$WindowsOnly,
    [switch]$SkipFeeder,
    [switch]$SkipInstallers,
    [switch]$SkipDocker,
    [switch]$Resources,
    [Alias('PiBase')][string]$PiImages,
    [string]$PiBaseSha256,
    [string]$PiImageKinds = 'station,feeder',
    [switch]$KeepRawImages,
    [string]$SignKey,
    [ValidateSet('stable', 'beta')][string]$Channel = 'stable',
    [int]$ManifestExpiresDays = 0,
    [string]$UpdateBaseUrl,
    [switch]$AllowTestKey
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

if (-not $OutDir) { $OutDir = Join-Path $root 'dist' }
New-Item -ItemType Directory -Force $OutDir | Out-Null
$dist = (Resolve-Path $OutDir).Path

# The version in file names; binaries are stamped only when -Version is given (else they carry model.Version).
$stamp = [bool]$Version
if (-not $Version) {
    $m = Select-String -Path (Join-Path $root 'internal\model\model.go') -Pattern '^const Version = "([^"]+)"'
    if (-not $m) { throw 'cannot read the version from internal\model\model.go; pass -Version' }
    $Version = $m.Matches[0].Groups[1].Value
}
if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$') { throw "invalid -Version '$Version' (MAJOR.MINOR.PATCH[-pre])" }
$xver = if ($stamp) { " -X main.version=$Version" } else { '' }

if ($SignKey) {
    if (-not (Test-Path -LiteralPath $SignKey -PathType Leaf)) { throw "-SignKey: $SignKey not found" }
    $SignKey = (Resolve-Path -LiteralPath $SignKey).Path
    if ($SignKey -ilike ($root.TrimEnd('\') + '\*')) { throw "-SignKey must not be inside the repository ($SignKey): keep the release key outside it" }
    # DLV-04: a throwaway key in a temporary folder must not sign a release: every station and feeder
    # configured with its public key could then only ever accept updates signed by that lost key.
    $tempDirs = @($env:TEMP, $env:TMP, [IO.Path]::GetTempPath()) | Where-Object { $_ } | ForEach-Object { $_.TrimEnd('\') }
    $inTemp = @($tempDirs | Where-Object { $SignKey -ilike ($_ + '\*') }).Count -gt 0
    $testName = (Split-Path -Leaf $SignKey) -imatch 'test|throwaway|scratch|dummy'
    if (($inTemp -or $testName) -and -not $AllowTestKey) {
        throw "-SignKey $SignKey looks like a test key (temporary folder or test name). Generate the real release key with 'go run ./deploy/release keygen', keep it offline with a backup, and pass that; or add -AllowTestKey for a test build."
    }
    if ($inTemp -or $testName) { Write-Host "WARNING: signing with a TEST key ($SignKey): not for publication" -ForegroundColor Yellow }
}
if ($UpdateBaseUrl) {
    $UpdateBaseUrl = $UpdateBaseUrl.TrimEnd('/')
    if ($UpdateBaseUrl -notmatch '^https://[^\s"]+$') { throw "-UpdateBaseUrl must be an https:// URL" }
    if (-not $SignKey) { throw '-UpdateBaseUrl needs -SignKey (feeders only accept signed updates)' }
}
$buildStart = Get-Date
if ($PiImages) {
    if ($WindowsOnly) { throw '-PiImages needs the Linux binaries; do not combine it with -WindowsOnly' }
    if (-not (Test-Path -LiteralPath $PiImages -PathType Leaf)) { throw "-PiImages: base image $PiImages not found" }
    $PiImages = (Resolve-Path -LiteralPath $PiImages).Path
}

$env:CGO_ENABLED = '0'
$common = @('-trimpath')
$work = Join-Path $dist '_work'
Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force $work | Out-Null
$artifacts = New-Object System.Collections.Generic.List[string]
function Add-Artifact([string]$Name) {
    if (-not (Test-Path -LiteralPath (Join-Path $dist $Name))) { throw "expected artifact $Name was not produced" }
    if (-not $artifacts.Contains($Name)) { $artifacts.Add($Name) }
}

function Invoke-Go {
    param([string[]]$GoArgs)
    & go @GoArgs
    if ($LASTEXITCODE -ne 0) { throw "go $($GoArgs -join ' ') failed with exit code $LASTEXITCODE" }
}

function Clear-Target {
    Remove-Item Env:GOOS, Env:GOARCH, Env:GOARM -ErrorAction SilentlyContinue
}

# Build-Go builds one package for one target. $Arch 'armv7' means GOARCH=arm GOARM=7.
function Build-Go {
    param([string]$Os, [string]$Arch, [string]$Pkg, [string]$Out, [string]$LdFlags = '-s -w', [switch]$NoVersion)
    $env:GOOS = $Os
    if ($Arch -eq 'armv7') { $env:GOARCH = 'arm'; $env:GOARM = '7' } else { $env:GOARCH = $Arch }
    $ld = if ($NoVersion) { $LdFlags } else { $LdFlags + $xver }
    try {
        Invoke-Go (@('build') + $common + @('-ldflags', $ld, '-o', $Out, $Pkg))
    } finally { Clear-Target }
}

# relkit (deploy\relkit): packing, checksums, notices and the final check.
Clear-Target
$relkit = Join-Path $work 'relkit.exe'
Invoke-Go @('build', '-o', $relkit, './deploy/relkit')
function Invoke-Relkit {
    param([string[]]$KitArgs)
    & $relkit @KitArgs
    if ($LASTEXITCODE -ne 0) { throw "relkit $($KitArgs[0]) failed ($LASTEXITCODE)" }
}

# The release key's public half is baked into the feeder installers and the feeder image (DLV-03), so it is
# read before they are built.
$pub = ''
$feederUpdateUrl = ''
if ($SignKey) {
    $relTool = Join-Path $work 'release.exe'
    Invoke-Go @('build', '-o', $relTool, './deploy/release')
    $pub = (& $relTool pubkey -key $SignKey | Select-Object -First 1)
    if ($LASTEXITCODE -ne 0 -or -not $pub) { throw 'cannot read the public key of -SignKey' }
    $pub = $pub.Trim()
    if ($UpdateBaseUrl) { $feederUpdateUrl = "$UpdateBaseUrl/feeder/manifest.json" }
    Write-Host "release public key $pub (baked into the feeder installers and image)"
}

# New-Bundle packs $work\$Name as $dist\$Name.tar.gz (top-level directory $Name).
function New-Bundle {
    param([string]$Name)
    $out = Join-Path $dist "$Name.tar.gz"
    Invoke-Relkit @('pack', '-dir', (Join-Path $work $Name), '-o', $out)
    Add-Artifact "$Name.tar.gz"
}

# Copy-Text copies a text file with LF line endings (a CRLF checkout must not break sh).
function Copy-Text {
    param([string]$From, [string]$To)
    New-Item -ItemType Directory -Force (Split-Path -Parent $To) | Out-Null
    $text = [IO.File]::ReadAllText($From) -replace "`r`n", "`n"
    [IO.File]::WriteAllText($To, $text, (New-Object Text.UTF8Encoding $false))
}

function Set-Text {
    param([string]$To, [string]$Text)
    [IO.File]::WriteAllText($To, $Text, (New-Object Text.UTF8Encoding $false))
}

# New-Stage creates an empty staging directory $work\$Name.
function New-Stage {
    param([string]$Name)
    $stage = Join-Path $work $Name
    Remove-Item -Recurse -Force $stage -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Force $stage | Out-Null
    return $stage
}

# Bundle extras: VERSION, licence terms and the third-party notices of the program inside.
$licence = Join-Path $root 'deploy\installer\LICENSE.txt'
function Add-Extras {
    param([string]$Stage, [string]$Notices)
    Set-Text (Join-Path $Stage 'VERSION') "$Version`n"
    if (Test-Path $licence) { Copy-Text $licence (Join-Path $Stage 'LICENSE.txt') }
    Copy-Item $Notices (Join-Path $Stage 'THIRD-PARTY-NOTICES.txt')
}

if ($Resources) {
    Write-Host '== Windows resources (icon, version info, manifest) ==' -ForegroundColor Cyan
    Push-Location (Join-Path $root 'cmd\airview\winres')
    try { Invoke-Go @('run', 'gen_icon.go') } finally { Pop-Location }
    Push-Location (Join-Path $root 'cmd\airview')
    try {
        Invoke-Go @('run', 'github.com/tc-hib/go-winres@v0.3.3', 'make', '--in', 'winres/winres.json', '--out', 'rsrc', '--arch', 'amd64,arm64')
    } finally { Pop-Location }
    if (Test-Path (Join-Path $root 'cmd\airview-feeder\winres\winres.json')) {
        Invoke-Go @('run', './deploy/feeder/windows/mkicon', '-o', 'cmd/airview-feeder/winres/icon.ico', '-png', '256', '-png-out', 'cmd/airview-feeder/winres/icon.png')
        Push-Location (Join-Path $root 'cmd\airview-feeder')
        try {
            Invoke-Go @('run', 'github.com/tc-hib/go-winres@v0.3.3', 'make', '--in', 'winres/winres.json', '--out', 'rsrc', '--arch', 'amd64,arm64')
        } finally { Pop-Location }
    }
}

if ($Resources) { $buildStart = Get-Date } # the regenerated resources are part of the source state

if (-not $SkipTests) {
    Write-Host '== go vet / go test ==' -ForegroundColor Cyan
    Invoke-Go @('vet', './...')
    Invoke-Go @('test', './...')
}

$feederPkg = Join-Path $root 'cmd\airview-feeder'
$buildFeeder = -not $SkipFeeder
if ($buildFeeder -and -not (Test-Path (Join-Path $feederPkg '*.go'))) {
    Write-Host 'cmd\airview-feeder does not exist: the feeder artifacts are skipped' -ForegroundColor Yellow
    $buildFeeder = $false
}

Write-Host "AirView $Version release build -> $dist" -ForegroundColor Green

# ---------------------------------------------------------------------------------------------- Windows --
Write-Host '== windows/amd64 (GUI) ==' -ForegroundColor Cyan
$exe = Join-Path $dist 'airview.exe'
# A running airview.exe cannot be overwritten, but Windows lets us rename it out of the way.
Remove-Item "$exe.old" -Force -ErrorAction SilentlyContinue
if (Test-Path $exe) {
    try { [IO.File]::Open($exe, 'Open', 'ReadWrite', 'None').Close() }
    catch {
        Write-Host "$exe is in use; renaming it to airview.exe.old" -ForegroundColor Yellow
        Rename-Item $exe "$exe.old" -Force
    }
}
Build-Go 'windows' 'amd64' './cmd/airview' $exe '-H windowsgui -s -w'
Add-Artifact 'airview.exe'

if ($buildFeeder) {
    foreach ($arch in 'amd64', 'arm64') {
        Write-Host "== airview-feeder windows/$arch ==" -ForegroundColor Cyan
        Build-Go 'windows' $arch './cmd/airview-feeder' (Join-Path $dist "airview-feeder-windows-$arch.exe")
        Add-Artifact "airview-feeder-windows-$arch.exe"
    }
    # Portable ZIPs (deploy\feeder\windows\build.ps1, WP-WF). Its Inno Setup installer is superseded by the
    # own installers below (-NoInstaller).
    Write-Host '== portable Windows feeder ZIPs ==' -ForegroundColor Cyan
    $wfOut = Join-Path $work 'feeder-windows'
    $wfArgs = @{ OutDir = $wfOut; ReleaseStage = $true; NoInstaller = $true; Version = $Version }
    & (Join-Path $root 'deploy\feeder\windows\build.ps1') @wfArgs
    foreach ($f in Get-ChildItem $wfOut -File -Filter '*.zip') {
        Move-Item -Force $f.FullName (Join-Path $dist $f.Name)
        Add-Artifact $f.Name
    }
}

if (-not $SkipInstallers) {
    # Own pure-Go Windows installers (WP-SETUP-exe) from the binaries just built. Unsigned (DESIGN section 19).
    Write-Host '== Windows installers ==' -ForegroundColor Cyan
    $instArgs = @{ OutDir = $dist; ReleaseStage = $true; BinDir = $dist; Version = $Version }
    if ($Resources) { $instArgs.Resources = $true }
    if (-not $buildFeeder) { $instArgs.SkipFeeder = $true }
    if ($pub) { $instArgs.ManifestKey = $pub; if ($feederUpdateUrl) { $instArgs.UpdateUrl = $feederUpdateUrl } }
    & (Join-Path $root 'deploy\installer\build-installers.ps1') @instArgs
    Add-Artifact "AirView-Setup-$Version.exe"
    if ($buildFeeder) {
        Add-Artifact "AirView-Feeder-Setup-$Version.exe"
        Add-Artifact "AirView-Feeder-Setup-$Version-arm64.exe"
    }
}

# ------------------------------------------------------------------------------------------------ Linux --
if (-not $WindowsOnly) {
    foreach ($arch in 'arm64', 'armv7', 'amd64') {
        Write-Host "== airview linux/$arch ==" -ForegroundColor Cyan
        Build-Go 'linux' $arch './cmd/airview' (Join-Path $dist "airview-linux-$arch")
        Add-Artifact "airview-linux-$arch"
    }
    foreach ($arch in 'amd64', 'arm64') {
        Build-Go 'linux' $arch './deploy/server/healthcheck' (Join-Path $work "airview-healthcheck-linux-$arch") -NoVersion
    }

    Write-Host '== third-party notices ==' -ForegroundColor Cyan
    $noticesAirview = Join-Path $work 'notices-airview.txt'
    Invoke-Relkit @('notices', '-pkg', './cmd/airview', '-goos', 'linux', '-goarch', 'arm64', '-web', (Join-Path $root 'web\static\vendor'), '-o', $noticesAirview)

    # Raspberry Pi station app.
    Write-Host '== Pi station bundle ==' -ForegroundColor Cyan
    $piName = "airview-pi-$Version"
    $stage = New-Stage $piName
    Copy-Item (Join-Path $dist 'airview-linux-arm64') $stage
    Copy-Item (Join-Path $dist 'airview-linux-armv7') $stage
    foreach ($f in 'install.sh', 'update.sh', 'uninstall.sh', 'airview.service', 'README.md') {
        Copy-Text (Join-Path $root "deploy\pi\$f") (Join-Path $stage $f)
    }
    Add-Extras $stage $noticesAirview
    New-Bundle $piName
    Copy-Text (Join-Path $root 'deploy\pi\install.sh') (Join-Path $dist 'airview-pi-install.sh')
    Add-Artifact 'airview-pi-install.sh'

    # Linux server kits (deploy\server, DESIGN section 20.1). The names stay unversioned: the one-line
    # installer (airview-server-install.sh --base-url) fetches airview-server-linux-<arch>.tar.gz.
    $serverSrc = Join-Path $root 'deploy\server'
    foreach ($arch in 'amd64', 'arm64') {
        Write-Host "== server bundle linux/$arch ==" -ForegroundColor Cyan
        $name = "airview-server-linux-$arch"
        $stage = New-Stage $name
        Copy-Item (Join-Path $dist "airview-linux-$arch") (Join-Path $stage 'airview')
        foreach ($f in Get-ChildItem $serverSrc -File -Filter '*.sh') { Copy-Text $f.FullName (Join-Path $stage $f.Name) }
        foreach ($f in 'airview.service', 'airview.logrotate', 'README.md', 'FIREWALL.md') {
            Copy-Text (Join-Path $serverSrc $f) (Join-Path $stage $f)
        }
        foreach ($f in Get-ChildItem (Join-Path $serverSrc 'proxy') -File) { Copy-Text $f.FullName (Join-Path $stage "proxy\$($f.Name)") }
        Add-Extras $stage $noticesAirview
        New-Bundle $name
    }
    Copy-Text (Join-Path $serverSrc 'install.sh') (Join-Path $dist 'airview-server-install.sh')
    Add-Artifact 'airview-server-install.sh'

    if ($buildFeeder) {
        foreach ($arch in 'arm64', 'armv7', 'amd64') {
            Write-Host "== airview-feeder linux/$arch ==" -ForegroundColor Cyan
            Build-Go 'linux' $arch './cmd/airview-feeder' (Join-Path $dist "airview-feeder-linux-$arch")
            Add-Artifact "airview-feeder-linux-$arch"
        }
        $noticesFeeder = Join-Path $work 'notices-feeder.txt'
        Invoke-Relkit @('notices', '-pkg', './cmd/airview-feeder', '-goos', 'linux', '-goarch', 'arm64', '-o', $noticesFeeder)

        Write-Host '== feeder Pi bundle ==' -ForegroundColor Cyan
        $fpName = "airview-feeder-pi-$Version"
        $stage = New-Stage $fpName
        foreach ($arch in 'arm64', 'armv7', 'amd64') { Copy-Item (Join-Path $dist "airview-feeder-linux-$arch") $stage }
        $feederPi = (Resolve-Path (Join-Path $root 'deploy\feeder\pi')).Path
        foreach ($f in Get-ChildItem $feederPi -Recurse -File) {
            $rel = $f.FullName.Substring($feederPi.Length + 1)
            # Go tests of the installer and their fixtures are not part of the bundle.
            if ($f.Extension -eq '.go' -or $rel -match '(^|\\)testdata(\\|$)') { continue }
            $to = Join-Path $stage $rel
            if ($f.Extension -in '.sh', '.service', '.timer', '.md', '.json', '.conf', '.txt', '') { Copy-Text $f.FullName $to }
            else { New-Item -ItemType Directory -Force (Split-Path -Parent $to) | Out-Null; Copy-Item $f.FullName $to }
        }
        Add-Extras $stage $noticesFeeder
        New-Bundle $fpName
        Copy-Text (Join-Path $feederPi 'install.sh') (Join-Path $dist 'airview-feeder-install.sh')
        Add-Artifact 'airview-feeder-install.sh'
    }

    if (-not $SkipDocker) {
        Write-Host '== Docker build context ==' -ForegroundColor Cyan
        $dkName = "airview-docker-$Version"
        $stage = New-Stage $dkName
        Copy-Text (Join-Path $root 'deploy\docker\Dockerfile') (Join-Path $stage 'Dockerfile')
        Copy-Text (Join-Path $root 'deploy\docker\README.md') (Join-Path $stage 'README.md')
        Copy-Text (Join-Path $root 'docker-compose.example.yml') (Join-Path $stage 'docker-compose.example.yml')
        New-Item -ItemType Directory -Force (Join-Path $stage 'bin'), (Join-Path $stage 'data'), (Join-Path $stage 'etc-airview') | Out-Null
        foreach ($arch in 'amd64', 'arm64') {
            Copy-Item (Join-Path $dist "airview-linux-$arch") (Join-Path $stage 'bin')
            Copy-Item (Join-Path $work "airview-healthcheck-linux-$arch") (Join-Path $stage 'bin')
        }
        Add-Extras $stage $noticesAirview
        New-Bundle $dkName
    }
}

# ----------------------------------------------------------------------------------- Raspberry Pi images --
if ($PiImages) {
    # SD-card images (WP-PIIMG): Raspberry Pi OS Lite + AirView, first boot via cloud-init.
    Write-Host '== Raspberry Pi images ==' -ForegroundColor Cyan
    if ($PiBaseSha256) {
        $h = (Get-FileHash -Algorithm SHA256 -LiteralPath $PiImages).Hash.ToLowerInvariant()
        if ($h -ne $PiBaseSha256.ToLowerInvariant()) { throw "SHA-256 of $PiImages is $h, expected $PiBaseSha256" }
        Write-Host "base image SHA-256 verified: $h"
    } else {
        Write-Host 'no -PiBaseSha256: the base image is not verified by this build (verify it against the published hash)' -ForegroundColor Yellow
    }
    if ($PiImageKinds -match 'feeder' -and -not $buildFeeder) { throw 'the feeder image needs the feeder binaries (no -SkipFeeder)' }
    $pisetup = Join-Path $work 'airview-pisetup'
    Build-Go 'linux' 'arm64' './cmd/airview-piimage/pisetup' $pisetup -NoVersion
    $piTool = Join-Path $work 'airview-piimage.exe'
    Invoke-Go @('build', '-trimpath', '-o', $piTool, './cmd/airview-piimage')
    $piOut = Join-Path $work 'pi-image'
    $a = @('build', '-base', $PiImages, '-out', $piOut, '-kinds', $PiImageKinds, '-repo', $root, '-version', $Version,
        '-airview', (Join-Path $dist 'airview-linux-arm64'), '-pisetup', $pisetup, "-keep-img=$(if ($KeepRawImages) { 'true' } else { 'false' })")
    if ($buildFeeder) { $a += @('-feeder', (Join-Path $dist 'airview-feeder-linux-arm64'), '-notices-feeder', $noticesFeeder) }
    # Licence texts of the programs inside (DLV-05) and the release key / URL for image feeders (DLV-03).
    $a += @('-licence', $licence, '-notices-station', $noticesAirview)
    if ($pub) { $a += @('-manifest-key', $pub); if ($UpdateBaseUrl) { $a += @('-update-url', $UpdateBaseUrl) } }
    & $piTool @a
    if ($LASTEXITCODE -ne 0) { throw "airview-piimage build failed ($LASTEXITCODE)" }
    foreach ($f in Get-ChildItem $piOut -File | Where-Object { $_.Name -like '*.img.zip' -or $_.Name -like '*.sha256' -or $_.Name -like '*.verify.txt' -or ($KeepRawImages -and $_.Name -like '*.img') }) {
        Move-Item -Force $f.FullName (Join-Path $dist $f.Name)
        if ($f.Name -notlike '*.sha256') { Add-Artifact $f.Name }
    }
}

# ------------------------------------------------------------------------------------------ PDF manual --
# The manual is built separately (node docs/manual/build/build-manual.mjs); when it exists it ships with the
# release and is listed in SHA256SUMS like every other download.
foreach ($m in Get-ChildItem (Join-Path $root 'docs\manual') -Filter 'AirView-Manual-*.pdf' -File -ErrorAction SilentlyContinue) {
    Copy-Item -Force $m.FullName (Join-Path $dist $m.Name)
    Add-Artifact $m.Name
}

# ------------------------------------------------------------------------------------- signed manifests --
if ($SignKey) {
    Write-Host '== signed update manifests ==' -ForegroundColor Cyan
    $products = @(@{ P = 'airview'; Dir = 'airview' })
    if ($buildFeeder) { $products += @{ P = 'airview-feeder'; Dir = 'feeder' } }
    foreach ($p in $products) {
        $relOut = Join-Path $dist "release\$($p.Dir)"
        $pa = @('publish', '-key', $SignKey, '-product', $p.P, '-version', $Version, '-channel', $Channel, '-dist', $dist, '-out', $relOut,
            '-notes', "AirView $Version")
        if ($ManifestExpiresDays -gt 0) { $pa += @('-expires-days', "$ManifestExpiresDays") }
        & $relTool @pa
        if ($LASTEXITCODE -ne 0) { throw "release publish $($p.P) failed ($LASTEXITCODE)" }
        $mdir = if ($Channel -eq 'beta') { Join-Path $relOut 'beta' } else { $relOut }
        & $relTool verify -pub $pub -in (Join-Path $mdir 'manifest.json') -dir $mdir
        if ($LASTEXITCODE -ne 0) { throw "release verify $($p.P) failed ($LASTEXITCODE)" }
    }
    Write-Host "manifests signed with public key $pub (put this value in update.keys / --manifest-key)"
}

# ------------------------------------------------------------------------------------ checksums + check --
Write-Host '== SHA256SUMS ==' -ForegroundColor Cyan
$list = ($artifacts | Sort-Object) -join ','
Invoke-Relkit @('sums', '-dir', $dist, '-files', $list)
if ($SignKey) {
    # SEC-R6: the Pi station installer (airview-pi-install.sh --base-url, update.sh) verifies
    # SHA256SUMS against the pinned release key before trusting any bundle it lists.
    & $relTool sign -raw -key $SignKey -in (Join-Path $dist 'SHA256SUMS') -out (Join-Path $dist 'SHA256SUMS.sig')
    if ($LASTEXITCODE -ne 0) { throw 'signing SHA256SUMS failed' }
}

Write-Host '== verify every artifact ==' -ForegroundColor Cyan
& $relkit check -dir $dist -version $Version -require $list
if ($LASTEXITCODE -ne 0) { throw "release check failed: see the problems above (work files kept in $work)" }

# DLV-01: the artifacts must match one state of the source tree. A file edited while the build ran may or may
# not be in a given binary, so such a build is refused (re-run it once nobody is editing).
Write-Host '== source tree unchanged during the build ==' -ForegroundColor Cyan
$srcRoots = @('cmd', 'internal', 'web', 'deploy') | ForEach-Object { Join-Path $root $_ } | Where-Object { Test-Path $_ }
$changed = @(Get-ChildItem -LiteralPath $srcRoots -Recurse -File -ErrorAction SilentlyContinue |
        Where-Object { $_.LastWriteTime -gt $buildStart -and $_.FullName -notlike "$dist\*" -and $_.FullName -notmatch '\\(out|_work|work)\\' -and $_.Name -notlike '*.exe' } |
        Select-Object -First 20)
foreach ($f in 'go.mod', 'go.sum') {
    $p = Join-Path $root $f
    if ((Test-Path $p) -and (Get-Item $p).LastWriteTime -gt $buildStart) { $changed += Get-Item $p }
}
if ($changed.Count -gt 0) {
    $changed | ForEach-Object { Write-Host "  changed during the build: $($_.FullName.Substring($root.Length + 1))" -ForegroundColor Red }
    throw "source files changed while the build ran (started $($buildStart.ToString('HH:mm:ss'))): the artifacts may not match the code; re-run the build"
}
Set-Text (Join-Path $dist 'BUILD-INFO.txt') ("AirView $Version`nbuilt $((Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')) from the source tree as of $($buildStart.ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')) (unchanged during the build)`n" + $(if ($pub) { "release public key $pub`n" } else { "unsigned (no -SignKey)`n" }))

Remove-Item -Recurse -Force $work
Write-Host ''
Write-Host "AirView ${Version}: $($artifacts.Count) artifacts in $dist" -ForegroundColor Green
if ($SignKey) { Write-Host "Signed update releases: $(Join-Path $dist 'release')" -ForegroundColor Green }
