# Acceptance test of the AirView installers (WP-SETUP-exe) on a Windows machine, without administrator rights
# and without touching any real installation.
#
#   powershell -ExecutionPolicy Bypass -File deploy\installer\test-installers.ps1 -Scratch <empty dir> [-Shots]
#
# It builds the release stub (as-invoker since DLV-06, so the same stub as a release) and both installers with a SCRATCH identity:
#   product ids  AirViewSetupTest / AirViewFeederSetupTest (HKCU uninstall keys of those names),
#   names        "AirView SetupTest" / "AirView Feeder SetupTest",
#   service      AirViewFeederSetupTest, tray Run value "AirView Feeder SetupTest Tray",
#   data         <Scratch>\data\*, Start menu and desktop redirected to <Scratch>\folders\* through the
#                AIRVIEW_SETUP_FOLDER_* overrides,
# then, per product: silent per-user install (/S /CURRENTUSER /D=<Scratch>\inst\...), verification of files
# (SHA-256 against the receipt), shortcuts (read back through WScript.Shell), the uninstall entry and the tray
# Run value; an in-place upgrade that must keep planted user data; the uninstaller (/S, and /REMOVEDATA for the
# feeder); verification of clean removal (files, folder, registry, shortcuts, Run value, no service). A
# per-machine silent install without administrator rights must exit 3. With -Shots, every wizard page of the
# installers with their real names is rendered to <Scratch>\shots (/SHOTS=, nothing installed).
# Exit code 0 when every check passed.
param(
    [Parameter(Mandatory = $true)][string]$Scratch,
    [switch]$Shots,
    [switch]$KeepBuild
)

$ErrorActionPreference = 'Stop'
$here = $PSScriptRoot
$root = (Resolve-Path (Join-Path $here '..\..')).Path
New-Item -ItemType Directory -Force $Scratch | Out-Null
$Scratch = (Resolve-Path $Scratch).Path
if ($Scratch -ilike "$root\dist*") { throw 'refusing to test inside dist' }

$script:failures = 0
function Check([bool]$Ok, [string]$What) {
    if ($Ok) { Write-Host "  ok    $What" -ForegroundColor Green }
    else { Write-Host "  FAIL  $What" -ForegroundColor Red; $script:failures++ }
}

function Invoke-Go([string[]]$GoArgs, [hashtable]$Env = @{}) {
    $saved = @{}
    foreach ($k in $Env.Keys) { $saved[$k] = [Environment]::GetEnvironmentVariable($k); [Environment]::SetEnvironmentVariable($k, $Env[$k]) }
    try {
        Push-Location $root
        & go @GoArgs
        if ($LASTEXITCODE -ne 0) { throw "go $($GoArgs -join ' ') failed" }
    } finally {
        Pop-Location
        foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
    }
}

# ---- build -------------------------------------------------------------------
$build = Join-Path $Scratch 'build'
New-Item -ItemType Directory -Force $build | Out-Null
$win = @{ CGO_ENABLED = '0'; GOOS = 'windows'; GOARCH = 'amd64' }
Write-Host '== build ==' -ForegroundColor Cyan
Invoke-Go @('build', '-o', "$build\mkinstaller.exe", './cmd/airview-setup/mkinstaller') @{ CGO_ENABLED = '0'; GOOS = ''; GOARCH = '' }
Invoke-Go @('build', '-trimpath', '-ldflags', '-H windowsgui -s -w', '-o', "$build\stub-user.exe", './cmd/airview-setup') $win
Invoke-Go @('build', '-trimpath', '-ldflags', '-H windowsgui -s -w', '-o', "$build\airview.exe", './cmd/airview') $win
Invoke-Go @('build', '-trimpath', '-ldflags', '-s -w', '-o', "$build\airview-feeder.exe", './cmd/airview-feeder') $win
Invoke-Go @('build', '-trimpath', '-ldflags', '-H windowsgui -s -w', '-o', "$build\airview-feeder-tray.exe", './cmd/airview-feeder') $win
Invoke-Go @('run', './deploy/feeder/windows/mkicon', '-o', "$build\airview-feeder.ico") @{ CGO_ENABLED = '0'; GOOS = ''; GOARCH = '' }
'test notices' | Set-Content -Encoding ascii "$build\NOTICES.txt"
$ver = '0.0.1'
$docs = Join-Path $root 'docs\aggregator'
$data = Join-Path $Scratch 'data'
$stationData = Join-Path $data 'station'
$feederData = Join-Path $data 'feeder'

function Build-Installer([string]$Product, [string]$Out, [string[]]$Extra) {
    $common = @('-stub', "$build\stub-user.exe", '-product', (Join-Path $here "products\$Product.json"), '-version', $ver, '-out', $Out,
        '-file', "LICENSE.txt=$(Join-Path $here 'LICENSE.txt')", '-file', "THIRD-PARTY-NOTICES.txt=$build\NOTICES.txt",
        '-file', "docs/PRIVACY-NOTICE.md=$(Join-Path $docs 'PRIVACY-NOTICE.md')")
    if ($Product -eq 'station') {
        $common += @('-file', "airview.exe=$build\airview.exe", '-file', "README.txt=$(Join-Path $here 'README-station.txt')")
    } else {
        $common += @('-file', "airview-feeder.exe=$build\airview-feeder.exe", '-file', "airview-feeder-tray.exe=$build\airview-feeder-tray.exe",
            '-file', "airview-feeder.ico=$build\airview-feeder.ico", '-file', "README.txt=$(Join-Path $root 'deploy\feeder\windows\installer\README.txt')")
    }
    & "$build\mkinstaller.exe" @($common + $Extra)
    if ($LASTEXITCODE -ne 0) { throw "mkinstaller $Product failed" }
}

$stationSetup = Join-Path $build 'AirView-SetupTest-Setup.exe'
$feederSetup = Join-Path $build 'AirView-Feeder-SetupTest-Setup.exe'
Build-Installer 'station' $stationSetup @('-id', 'AirViewSetupTest', '-name', 'AirView SetupTest', '-data-dir', $stationData)
Build-Installer 'feeder' $feederSetup @('-id', 'AirViewFeederSetupTest', '-name', 'AirView Feeder SetupTest',
    '-var', 'service=AirViewFeederSetupTest', '-var', 'tray=AirView Feeder SetupTest Tray', '-var', "datadir=$feederData")

# ---- helpers -------------------------------------------------------------------
$folders = Join-Path $Scratch 'folders'
$env:AIRVIEW_SETUP_FOLDER_PROGRAMS = Join-Path $folders 'programs'
$env:AIRVIEW_SETUP_FOLDER_DESKTOP = Join-Path $folders 'desktop'
$uninstRoot = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall'
$runKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
$shell = New-Object -ComObject WScript.Shell
$logs = Join-Path $Scratch 'logs'
New-Item -ItemType Directory -Force $logs | Out-Null

function Run-Exe([string]$Exe, [string]$ArgLine) {
    $p = Start-Process -FilePath $Exe -ArgumentList $ArgLine -Wait -PassThru
    return $p.ExitCode
}

function Wait-Gone([string]$Path, [int]$Seconds = 60) {
    for ($i = 0; $i -lt $Seconds * 2; $i++) {
        if (-not (Test-Path -LiteralPath $Path)) { return $true }
        Start-Sleep -Milliseconds 500
    }
    return $false
}

function Verify-Installed([string]$Id, [string]$Name, [string]$Dir, [string[]]$Links, [string]$MainExe) {
    $rcpt = Get-Content -Raw (Join-Path $Dir 'install-receipt.json') | ConvertFrom-Json
    $bad = 0
    foreach ($f in $rcpt.files) {
        $p = Join-Path $Dir ($f.path -replace '/', '\')
        if (-not (Test-Path $p) -or (Get-FileHash $p -Algorithm SHA256).Hash.ToLower() -ne $f.sha256) { $bad++ }
    }
    Check ($bad -eq 0 -and $rcpt.files.Count -gt 3) "$Name : $($rcpt.files.Count) files match the receipt (SHA-256)"
    Check (Test-Path (Join-Path $Dir 'uninstall.exe')) "$Name : uninstall.exe present"
    $k = Get-ItemProperty -LiteralPath "$uninstRoot\$Id" -ErrorAction SilentlyContinue
    Check ($null -ne $k) "$Name : uninstall entry HKCU\...\Uninstall\$Id"
    if ($k) {
        Check ($k.DisplayName -eq $Name -and $k.DisplayVersion -eq $ver -and $k.Publisher -eq 'AirView') "$Name : DisplayName/DisplayVersion/Publisher = $($k.DisplayName)/$($k.DisplayVersion)/$($k.Publisher)"
        Check ($k.UninstallString -eq ('"' + (Join-Path $Dir 'uninstall.exe') + '"')) "$Name : UninstallString = $($k.UninstallString)"
        Check ($k.QuietUninstallString -like '*uninstall.exe" /S') "$Name : QuietUninstallString"
        Check ($k.DisplayIcon -eq ((Join-Path $Dir $MainExe) + ',0')) "$Name : DisplayIcon = $($k.DisplayIcon)"
        Check ($k.EstimatedSize -gt 1000 -and $k.InstallLocation -eq $Dir -and $k.NoModify -eq 1) "$Name : EstimatedSize $($k.EstimatedSize) KB, InstallLocation, NoModify"
    }
    foreach ($l in $Links) {
        $parts = $l.Split('|')
        $lnk = $parts[0]; $want = $parts[1]
        if ($lnk -like '*.url') {
            Check ((Test-Path $lnk) -and ((Get-Content -Raw $lnk) -match [regex]::Escape("URL=$want"))) "$Name : $(Split-Path -Leaf $lnk) -> $want"
        } else {
            $ok = Test-Path $lnk
            $t = if ($ok) { $shell.CreateShortcut($lnk).TargetPath } else { '' }
            Check ($ok -and $t -ieq $want) "$Name : shortcut $(Split-Path -Leaf $lnk) -> $t"
        }
    }
    return $rcpt
}

function Verify-Removed([string]$Id, [string]$Name, [string]$Dir, [string[]]$Links) {
    Check (Wait-Gone $Dir) "$Name : install folder removed ($Dir)"
    Check (-not (Test-Path -LiteralPath "$uninstRoot\$Id")) "$Name : uninstall entry removed"
    $left = @($Links | ForEach-Object { $_.Split('|')[0] } | Where-Object { Test-Path $_ })
    Check ($left.Count -eq 0) "$Name : shortcuts removed $($left -join ', ')"
}

$failuresAtStart = $script:failures
# ---- station -------------------------------------------------------------------
Write-Host '== station: silent per-user install ==' -ForegroundColor Cyan
$sDir = Join-Path $Scratch 'inst\AirView SetupTest'
$sm = Join-Path $env:AIRVIEW_SETUP_FOLDER_PROGRAMS 'AirView'
$sLinks = @("$sm\AirView.lnk|$sDir\airview.exe", "$sm\AirView documentation.lnk|$sDir\docs",
    "$sm\Uninstall AirView SetupTest.lnk|$sDir\uninstall.exe", "$($env:AIRVIEW_SETUP_FOLDER_DESKTOP)\AirView.lnk|$sDir\airview.exe")
$code = Run-Exe $stationSetup "/S /CURRENTUSER /LOG=$logs\station-install.log /D=$sDir"
Check ($code -eq 0) "station install exit code $code"
$null = Verify-Installed 'AirViewSetupTest' 'AirView SetupTest' $sDir $sLinks 'airview.exe'
Check ((& "$sDir\airview.exe" -version 2>&1 | Out-String) -match '\d+\.\d+') 'installed airview.exe runs (-version)'

Write-Host '== station: upgrade in place keeps data ==' -ForegroundColor Cyan
New-Item -ItemType Directory -Force $stationData, "$sDir\data" | Out-Null
'user settings' | Set-Content "$stationData\airview.jsonc"
'db' | Set-Content "$sDir\data\marker.txt"
$code = Run-Exe $stationSetup "/S /CURRENTUSER /NODESKTOP /LOG=$logs\station-upgrade.log"
Check ($code -eq 0) "upgrade exit code $code (default folder = existing install)"
$null = Verify-Installed 'AirViewSetupTest' 'AirView SetupTest' $sDir ($sLinks | Select-Object -First 3) 'airview.exe'
Check (-not (Test-Path "$($env:AIRVIEW_SETUP_FOLDER_DESKTOP)\AirView.lnk")) 'upgrade with /NODESKTOP removed the desktop shortcut'
Check ((Test-Path "$stationData\airview.jsonc") -and (Test-Path "$sDir\data\marker.txt")) 'upgrade kept the user data'
Check ((Get-Content -Raw "$logs\station-upgrade.log") -match 'Upgrading') 'log records the upgrade'

Write-Host '== station: uninstall (keep data) ==' -ForegroundColor Cyan
$code = Run-Exe "$sDir\uninstall.exe" "/S /LOG=$logs\station-uninstall.log"
Check ($code -eq 0) "uninstall exit code $code"
# The data folder inside the install folder is kept, so only the program files must be gone.
$leftFiles = @(Get-ChildItem -LiteralPath $sDir -Recurse -File -ErrorAction SilentlyContinue | Where-Object { $_.FullName -notlike "$sDir\data\*" -and $_.Name -ne 'uninstall.exe' })
Check ($leftFiles.Count -eq 0) "program files removed (left: $($leftFiles.Count))"
Check (Wait-Gone "$sDir\uninstall.exe") 'uninstall.exe removed after exit'
Check ((Test-Path "$stationData\airview.jsonc") -and (Test-Path "$sDir\data\marker.txt")) 'user data kept without /REMOVEDATA'
Check (-not (Test-Path -LiteralPath "$uninstRoot\AirViewSetupTest")) 'station uninstall entry removed'
Check (-not (Test-Path $sm)) 'Start menu folder removed'
Remove-Item -Recurse -Force (Join-Path $Scratch 'inst\AirView SetupTest')  # our planted data
Remove-Item -Recurse -Force $stationData

# ---- feeder --------------------------------------------------------------------
Write-Host '== feeder: silent per-user install ==' -ForegroundColor Cyan
$fDir = Join-Path $Scratch 'inst\AirView Feeder SetupTest'
$fsm = Join-Path $env:AIRVIEW_SETUP_FOLDER_PROGRAMS 'AirView Feeder'
$fLinks = @("$fsm\AirView Feeder.lnk|$fDir\airview-feeder-tray.exe", "$fsm\Read me.lnk|$fDir\README.txt",
    "$fsm\Status page.url|http://127.0.0.1:8099/", "$fsm\Uninstall AirView Feeder SetupTest.lnk|$fDir\uninstall.exe")
$code = Run-Exe $feederSetup "/S /CURRENTUSER /LOG=$logs\feeder-install.log /D=$fDir"
Check ($code -eq 0) "feeder install exit code $code"
$null = Verify-Installed 'AirViewFeederSetupTest' 'AirView Feeder SetupTest' $fDir $fLinks 'airview-feeder-tray.exe'
$run = (Get-ItemProperty -LiteralPath $runKey -ErrorAction SilentlyContinue).'AirView Feeder SetupTest Tray'
Check ($run -eq ('"' + "$fDir\airview-feeder-tray.exe" + '"')) "tray autostart HKCU Run = $run"
Check ((& "$fDir\airview-feeder.exe" version 2>&1 | Out-String) -match '\d+\.\d+') 'installed airview-feeder.exe runs (version)'
Check ($null -eq (Get-Service -Name 'AirViewFeederSetupTest' -ErrorAction SilentlyContinue)) 'per-user install registers no service'

Write-Host '== feeder: uninstall with /REMOVEDATA ==' -ForegroundColor Cyan
New-Item -ItemType Directory -Force $feederData | Out-Null
'{}' | Set-Content "$feederData\feeder.json"
$code = Run-Exe "$fDir\uninstall.exe" "/S /REMOVEDATA /LOG=$logs\feeder-uninstall.log"
Check ($code -eq 0) "uninstall exit code $code"
Verify-Removed 'AirViewFeederSetupTest' 'AirView Feeder SetupTest' $fDir $fLinks
Check ($null -eq (Get-ItemProperty -LiteralPath $runKey -ErrorAction SilentlyContinue).'AirView Feeder SetupTest Tray') 'tray Run value removed'
Check (-not (Test-Path $feederData)) 'feeder data removed with /REMOVEDATA'
Check ($null -eq (Get-Service -Name 'AirViewFeederSetupTest' -ErrorAction SilentlyContinue)) 'no service left'
Check (-not (Test-Path $fsm)) 'Start menu folder removed'

# ---- refusals -------------------------------------------------------------------
Write-Host '== refusals ==' -ForegroundColor Cyan
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    $code = Run-Exe $stationSetup "/S /LOG=$logs\station-machine.log /D=$Scratch\inst\machine"
    Check ($code -eq 3) "per-machine silent install without administrator rights exits 3 (got $code)"
    Check (-not (Test-Path "$Scratch\inst\machine")) 'nothing written'
}
$code = Run-Exe $stationSetup "/S /CURRENTUSER /SERVICE /LOG=$logs\station-bad.log"
Check ($code -eq 4) "unknown/invalid switch exits 4 (got $code)"
$code = Run-Exe $stationSetup "/S /CURRENTUSER /LOG=$logs\station-root.log /D=C:\"
Check ($code -eq 1 -and -not (Test-Path -LiteralPath "$uninstRoot\AirViewSetupTest")) "drive root refused (exit $code)"

# ---- wizard, driven through its buttons ------------------------------------------
Write-Host '== station: interactive wizard (buttons pressed programmatically) ==' -ForegroundColor Cyan
Add-Type -Namespace AvTest -Name Win -MemberDefinition @'
[DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern IntPtr FindWindow(string cls, string title);
[DllImport("user32.dll")] public static extern IntPtr GetDlgItem(IntPtr h, int id);
[DllImport("user32.dll")] public static extern IntPtr SendMessage(IntPtr h, uint m, IntPtr w, IntPtr l);
[DllImport("user32.dll")] public static extern bool IsWindowEnabled(IntPtr h);
[DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern int GetWindowText(IntPtr h, System.Text.StringBuilder s, int n);
[DllImport("user32.dll", CharSet = CharSet.Unicode, EntryPoint = "SendMessageW")] public static extern IntPtr SendText(IntPtr h, uint m, IntPtr w, string l);
[DllImport("user32.dll", CharSet = CharSet.Unicode, EntryPoint = "SendMessageW")] public static extern IntPtr GetText(IntPtr h, uint m, IntPtr w, System.Text.StringBuilder l);
'@
function Find-Wizard([string]$Title) {
    for ($i = 0; $i -lt 100; $i++) {
        $h = [AvTest.Win]::FindWindow('AirViewSetupWizard', $Title)
        if ($h -ne [IntPtr]::Zero) { return $h }
        Start-Sleep -Milliseconds 200
    }
    throw "wizard window '$Title' not found"
}
function Press([IntPtr]$W, [int]$Id) {
    $b = [AvTest.Win]::GetDlgItem($W, $Id)
    if ($b -eq [IntPtr]::Zero) { throw "control $Id not found" }
    [void][AvTest.Win]::SendMessage($b, 0x00F5, [IntPtr]::Zero, [IntPtr]::Zero) # BM_CLICK
    Start-Sleep -Milliseconds 300
}
function Button-Text([IntPtr]$W, [int]$Id) {
    $sb = New-Object Text.StringBuilder 64
    [void][AvTest.Win]::GetWindowText([AvTest.Win]::GetDlgItem($W, $Id), $sb, 64)
    return $sb.ToString()
}
function Wait-Finish([IntPtr]$W) {
    for ($i = 0; $i -lt 300; $i++) { if ((Button-Text $W 1) -eq '&Finish') { return $true }; Start-Sleep -Milliseconds 200 }
    return $false
}
$gDir = Join-Path $Scratch 'inst\AirView SetupTest GUI'
$proc = Start-Process -FilePath $stationSetup -ArgumentList "/CURRENTUSER /NOLAUNCH /LOG=$logs\station-gui.log" -PassThru
$w = Find-Wizard 'AirView SetupTest 0.0.1 Setup'
Press $w 1                                              # Welcome -> Licence
Check (-not [AvTest.Win]::IsWindowEnabled([AvTest.Win]::GetDlgItem($w, 1))) 'licence page: Next disabled until accepted'
Press $w 11                                             # I accept
Check ([AvTest.Win]::IsWindowEnabled([AvTest.Win]::GetDlgItem($w, 1))) 'licence page: Next enabled after accepting'
Press $w 1                                              # -> Destination
# WM_SETTEXT (SetWindowText cannot change another process's controls), then read it back: the install must
# go to the scratch folder or not at all.
$edit = [AvTest.Win]::GetDlgItem($w, 40)
[void][AvTest.Win]::SendText($edit, 0x000C, [IntPtr]::Zero, $gDir)
$sbDir = New-Object Text.StringBuilder 1024
[void][AvTest.Win]::GetText($edit, 0x000D, [IntPtr]1024, $sbDir)
if ($sbDir.ToString() -ne $gDir) {
    Stop-Process -Id $proc.Id -Force
    throw "could not set the destination in the wizard (got '$($sbDir.ToString())'); stopped before installing"
}
Check $true "destination set to the scratch folder"
Press $w 1                                              # -> Options
Press $w 21                                             # untick the desktop shortcut (option 2)
Press $w 1                                              # -> Ready
Check ((Button-Text $w 1) -eq '&Install') 'ready page shows Install'
Press $w 1                                              # Install
Check (Wait-Finish $w) 'wizard reached the finished page'
Press $w 1                                              # Finish
$proc.WaitForExit(30000) | Out-Null
Check ($proc.ExitCode -eq 0) "wizard exit code $($proc.ExitCode)"
$null = Verify-Installed 'AirViewSetupTest' 'AirView SetupTest' $gDir ($sLinks[0..1] -replace [regex]::Escape($sDir), $gDir) 'airview.exe'
Check (-not (Test-Path "$($env:AIRVIEW_SETUP_FOLDER_DESKTOP)\AirView.lnk")) 'unticked desktop shortcut not created'

$proc = Start-Process -FilePath "$gDir\uninstall.exe" -ArgumentList "/LOG=$logs\station-gui-uninstall.log" -PassThru
$w = Find-Wizard 'AirView SetupTest Uninstall'
Check ((Button-Text $w 1) -eq '&Uninstall') 'uninstall wizard: confirm page'
Press $w 1
Check (Wait-Finish $w) 'uninstall wizard reached the finished page'
Press $w 1
$proc.WaitForExit(30000) | Out-Null
Check ($proc.ExitCode -eq 0) "uninstall wizard exit code $($proc.ExitCode)"
Verify-Removed 'AirViewSetupTest' 'AirView SetupTest' $gDir ($sLinks[0..1] -replace [regex]::Escape($sDir), $gDir)

# ---- screenshots ---------------------------------------------------------------
if ($Shots) {
    Write-Host '== wizard screenshots ==' -ForegroundColor Cyan
    $shotDir = Join-Path $Scratch 'shots'
    Remove-Item Env:AIRVIEW_SETUP_FOLDER_PROGRAMS, Env:AIRVIEW_SETUP_FOLDER_DESKTOP
    $realS = Join-Path $build 'AirView-Setup-shots.exe'
    $realF = Join-Path $build 'AirView-Feeder-Setup-shots.exe'
    Build-Installer 'station' $realS @()
    Build-Installer 'feeder' $realF @()
    foreach ($s in $realS, $realF) {
        $code = Run-Exe $s "/SHOTS=$shotDir"
        Check ($code -eq 0) "$(Split-Path -Leaf $s) /SHOTS exit $code"
    }
    $n = @(Get-ChildItem $shotDir -Filter *.png).Count
    Check ($n -ge 14) "$n wizard pages rendered to $shotDir"
}

if (-not $KeepBuild) { Remove-Item -Recurse -Force $build }
Write-Host ''
if ($script:failures -eq 0) { Write-Host 'ALL CHECKS PASSED' -ForegroundColor Green; exit 0 }
Write-Host "$($script:failures) CHECK(S) FAILED" -ForegroundColor Red
exit 1
