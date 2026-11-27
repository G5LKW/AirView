; AirView Feeder - Windows installer (Inno Setup 6.3 or newer)
; DESIGN §12.3, work package WP-WF.
;
; Built by deploy\feeder\windows\build.ps1, which stages the files and runs:
;
;   ISCC.exe /DArch=amd64 /DAppVersion=1.2.3 /DSrcDir=<stage> /O<outdir> airview-feeder.iss
;   (add /DSign=1 /Ssigntool="signtool.exe sign /fd sha256 /tr <tsa> /td sha256 /sha1 <thumbprint> $f"
;    to Authenticode-sign the installer and the uninstaller)
;
; What it does (per-machine install, needs administrator rights):
;   - copies airview-feeder.exe (console) and airview-feeder-tray.exe (no
;     console window) to {autopf}\AirView Feeder;
;   - optionally writes the first configuration (aggregator, position,
;     privacy) with "airview-feeder setup -non-interactive"; leave the server
;     empty to do it later on the local status page (browser-based setup);
;   - registers the Windows service AirViewFeeder (NT SERVICE\AirViewFeeder,
;     data in %ProgramData%\AirView Feeder, ACL-limited) and starts it;
;   - starts the tray icon at log-on for every user (HKLM Run);
;   - adds a firewall rule only when the status page is opened to the LAN.
; Upgrades keep the configuration and identity; uninstalling keeps them too
; unless the user asks for them to be deleted.
;
; Silent installs take the setup values as parameters, e.g.
;   AirView-Feeder-Setup.exe /VERYSILENT /SERVER=feed.example.org /PIN=sha256/... /ENROL=TOKEN
;     /NAME=home1 /LAT=51.5 /LON=-0.1 /ALT=35m /AGL=8m /PRIVACY=hidden [/NOTIMESTAMPS] [/NOLOCATION]
;     [/TASKS="service,trayautostart"] [/MANIFESTURL=https://host/feeder/manifest.json /MANIFESTKEY=<base64 key>]

#if VER < EncodeVer(6, 3, 0)
  #error Inno Setup 6.3 or newer is required (ExecAndCaptureOutput, x64compatible)
#endif

#ifndef Arch
  #define Arch "amd64"
#endif
#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif
#ifndef SrcDir
  #define SrcDir "out\stage-" + Arch
#endif

#define AppName "AirView Feeder"
#define ServiceName "AirViewFeeder"
#define FirewallRule "AirView Feeder status page"
; Must match trayRunValue in cmd\airview-feeder\tray_windows.go.
#define TrayRunValue "AirView Feeder Tray"
#define UpdateTask "AirView Feeder update"

[Setup]
AppId={{6C1F7E52-3B8D-4E0A-9C61-2F4A8D5B7E19}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
AppPublisher=AirView
AppComments=Feeds local ADS-B / Mode S receivers to AirView aggregators.
DefaultDirName={autopf}\{#AppName}
DefaultGroupName={#AppName}
DisableProgramGroupPage=yes
PrivilegesRequired=admin
#if Arch == "arm64"
ArchitecturesAllowed=arm64
ArchitecturesInstallIn64BitMode=arm64
#else
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
#endif
MinVersion=10.0
OutputBaseFilename=AirView-Feeder-Setup-{#AppVersion}-windows-{#Arch}
SetupIconFile={#SrcDir}\airview-feeder.ico
UninstallDisplayIcon={app}\airview-feeder.ico
UninstallDisplayName={#AppName}
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
; The tray and the service are closed by [Code] (PrepareToInstall), not by
; the Restart Manager: it cannot stop a service cleanly.
CloseApplications=no
RestartApplications=no
#ifdef Sign
SignTool=signtool
SignedUninstaller=yes
#endif

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[CustomMessages]
; C1 and C2 of docs\aggregator\CONSENT.md, word for word (%n = new line).
; deploy\feeder\windows\iss_test.go checks them against CONSENT.md and
; cmd\airview-feeder\consent.go.
C1=If your receiver sends timestamps, the network operator can determine its location precisely, whatever you enter. Other users and the public cannot. Choose 'No timestamps (no MLAT)' if you do not want this.
C2Title=Show my station on the public map?
C2Hidden=Hidden (default): no pin.
C2Region=Region: a point placed at random inside a 0.5-degree area around your station, about 55 by 35 km in the UK.
C2Area=Area: the same inside a 0.1-degree area, about 11 by 7 km. Where fewer than 3 stations are nearby it is shown as Region.
C2Note=The point is chosen once and stays put, so it cannot be averaged out. Your exact position is never public. You can change this at any time.

[Tasks]
Name: "service"; Description: "Run the feeder as a Windows &service (starts with Windows, feeds without anyone logged on)"
Name: "trayautostart"; Description: "Show the &tray icon when users log on"
Name: "autoupdate"; Description: "Install signed &updates automatically (daily task; needs the service; active once the network publishes updates)"
Name: "lanui"; Description: "Open the status page to other computers on the &local network (asks for a password)"; Flags: unchecked

[Files]
Source: "{#SrcDir}\airview-feeder.exe"; DestDir: "{app}"; Flags: ignoreversion restartreplace uninsrestartdelete
Source: "{#SrcDir}\airview-feeder-tray.exe"; DestDir: "{app}"; Flags: ignoreversion restartreplace uninsrestartdelete
Source: "{#SrcDir}\airview-feeder.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SrcDir}\README.txt"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SrcDir}\LICENSE.txt"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SrcDir}\THIRD-PARTY-NOTICES.txt"; DestDir: "{app}"; Flags: ignoreversion

[INI]
Filename: "{app}\Status page.url"; Section: "InternetShortcut"; Key: "URL"; String: "http://127.0.0.1:8099/"
Filename: "{app}\Status page.url"; Section: "InternetShortcut"; Key: "IconFile"; String: "{app}\airview-feeder.ico"
Filename: "{app}\Status page.url"; Section: "InternetShortcut"; Key: "IconIndex"; String: "0"

[Icons]
Name: "{autoprograms}\{#AppName}\{#AppName}"; Filename: "{app}\airview-feeder-tray.exe"; IconFilename: "{app}\airview-feeder.ico"; Comment: "Tray icon: status, pause, logs, settings"
Name: "{autoprograms}\{#AppName}\Status page"; Filename: "{app}\Status page.url"; IconFilename: "{app}\airview-feeder.ico"
Name: "{autoprograms}\{#AppName}\Logs"; Filename: "{commonappdata}\{#AppName}\logs"
Name: "{autoprograms}\{#AppName}\Uninstall {#AppName}"; Filename: "{uninstallexe}"

[Registry]
Root: HKLM; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "{#TrayRunValue}"; ValueData: """{app}\airview-feeder-tray.exe"""; Tasks: trayautostart; Flags: uninsdeletevalue

[Run]
Filename: "{app}\airview-feeder-tray.exe"; Description: "Start the tray icon now"; Flags: postinstall nowait runasoriginaluser skipifsilent
Filename: "{app}\Status page.url"; Description: "Open the status page (finish the setup there)"; Flags: postinstall shellexec nowait runasoriginaluser skipifsilent unchecked

[UninstallDelete]
Type: files; Name: "{app}\Status page.url"
Type: files; Name: "{app}\*.old"

[Code]
var
  AggPage: TInputQueryWizardPage;
  LocPage: TInputQueryWizardPage;
  TsPage, PinPage: TWizardPage;
  LanPage: TInputQueryWizardPage;
  TsYes, TsNo, PinHidden, PinRegion, PinArea: TNewRadioButton;
  NoLocation: TNewCheckBox;
  ConfigExisted: Boolean;
  { Redact is replaced by *** in the setup log (the status page password). }
  Redact: String;

function FeederExe: String;
begin
  Result := ExpandConstant('{app}\airview-feeder.exe');
end;

function DataDir: String;
begin
  Result := ExpandConstant('{commonappdata}\{#AppName}');
end;

{ Q quotes one command-line argument (values never contain quotes: see Valid). }
function Q(const S: String): String;
begin
  Result := '"' + S + '"';
end;

function Param(const Name, Def: String): String;
begin
  Result := ExpandConstant('{param:' + Name + '|' + Def + '}');
end;

function HasSwitch(const Name: String): Boolean;
var
  I: Integer;
begin
  Result := False;
  for I := 1 to ParamCount do
    if CompareText(ParamStr(I), '/' + Name) = 0 then
      Result := True;
end;

{ RunFeeder runs the console feeder hidden and logs its output. }
function RunFeeder(const Exe, Params: String; var Output: String): Integer;
var
  Code, I: Integer;
  Res: TExecOutput;
  Shown: String;
begin
  Output := '';
  Shown := Params;
  if Redact <> '' then
    StringChangeEx(Shown, Redact, '***', True);
  Log('Running: ' + Exe + ' ' + Shown);
  if not ExecAndCaptureOutput(Exe, Params, '', SW_HIDE, ewWaitUntilTerminated, Code, Res) then
  begin
    Result := -1;
    Output := SysErrorMessage(Code);
    Log('  could not start: ' + Output);
    exit;
  end;
  for I := 0 to GetArrayLength(Res.StdOut) - 1 do
    Output := Output + Res.StdOut[I] + #13#10;
  for I := 0 to GetArrayLength(Res.StdErr) - 1 do
    Output := Output + Res.StdErr[I] + #13#10;
  Log('  exit code ' + IntToStr(Code) + #13#10 + Output);
  Result := Code;
end;

function Valid(const S: String): Boolean;
begin
  Result := (Pos('"', S) = 0) and (Pos(#13, S) = 0) and (Pos(#10, S) = 0);
end;

{ AddText puts a word-wrapped label on a page and returns its bottom. }
function AddText(Page: TWizardPage; const Text: String; Left, Top: Integer; Bold: Boolean): Integer;
var
  L: TNewStaticText;
begin
  L := TNewStaticText.Create(Page);
  L.Parent := Page.Surface;
  L.AutoSize := False;
  L.WordWrap := True;
  L.Left := Left;
  L.Top := Top;
  L.Width := Page.SurfaceWidth - Left;
  L.Caption := Text;
  if Bold then
    L.Font.Style := [fsBold];
  L.AdjustHeight;
  Result := L.Top + L.Height;
end;

{ AddChoice adds a radio button whose text is a separate word-wrapped label
  (the consent texts must appear in full, whatever the window width). }
function AddChoice(Page: TWizardPage; const Text: String; var Top: Integer): TNewRadioButton;
begin
  Result := TNewRadioButton.Create(Page);
  Result.Parent := Page.Surface;
  Result.Left := ScaleX(8);
  Result.Top := Top;
  Result.Width := ScaleX(18);
  Result.Height := ScaleY(17);
  Top := AddText(Page, Text, ScaleX(30), Top + ScaleY(1), False) + ScaleY(8);
end;

procedure CreatePrivacyPages(AfterID: Integer);
var
  Y: Integer;
begin
  TsPage := CreateCustomPage(AfterID, 'Timestamps', 'Multilateration (MLAT) and your location');
  Y := AddText(TsPage, CustomMessage('C1'), 0, 0, False) + ScaleY(12);
  TsYes := AddChoice(TsPage, 'Send timestamps (MLAT)', Y);
  TsNo := AddChoice(TsPage, 'No timestamps (no MLAT)', Y);
  TsNo.Checked := HasSwitch('NOTIMESTAMPS');
  TsYes.Checked := not TsNo.Checked;

  PinPage := CreateCustomPage(TsPage.ID, 'Public map', 'Opt-in: off unless you choose otherwise');
  Y := AddText(PinPage, CustomMessage('C2Title'), 0, 0, True) + ScaleY(8);
  PinHidden := AddChoice(PinPage, CustomMessage('C2Hidden'), Y);
  PinRegion := AddChoice(PinPage, CustomMessage('C2Region'), Y);
  PinArea := AddChoice(PinPage, CustomMessage('C2Area'), Y);
  AddText(PinPage, CustomMessage('C2Note'), 0, Y + ScaleY(4), False);
  if CompareText(Param('PRIVACY', 'hidden'), 'region') = 0 then
    PinRegion.Checked := True
  else if CompareText(Param('PRIVACY', 'hidden'), 'cell01') = 0 then
    PinArea.Checked := True
  else
    PinHidden.Checked := True;
end;
procedure InitializeWizard;
begin
  ConfigExisted := FileExists(DataDir + '\feeder.json');

  AggPage := CreateInputQueryPage(wpSelectTasks, 'Aggregator',
    'Which AirView network should this feeder feed?',
    'Copy the values from the network''s "Feed this network" page. Leave the server empty to set the feeder up later on its status page (http://127.0.0.1:8099/).');
  if ConfigExisted then
    AggPage.SubCaptionLabel.Caption := 'This computer already has a feeder configuration. Leave the server empty to keep it unchanged, or enter a server to change the first aggregator (other values are kept unless you enter new ones).';
  AggPage.Add('Server (host or host:port):', False);
  AggPage.Add('Server pin (sha256/..., optional):', False);
  AggPage.Add('Enrol token (only for invite-only networks):', False);
  AggPage.Add('Station name (letters, digits, - and _; at most 32):', False);
  AggPage.Values[0] := Param('SERVER', '');
  AggPage.Values[1] := Param('PIN', '');
  AggPage.Values[2] := Param('ENROL', '');
  AggPage.Values[3] := Param('NAME', '');

  LocPage := CreateInputQueryPage(AggPage.ID, 'Antenna position',
    'Where is the antenna?',
    'An exact position (from a map or GPS) gives the best results. Altitude needs a unit, for example 120m or 400ft (above sea level). ' + CustomMessage('C1'));
  LocPage.Add('Latitude (decimal degrees, e.g. 51.4775):', False);
  LocPage.Add('Longitude (decimal degrees, e.g. -0.4614):', False);
  LocPage.Add('Antenna altitude above sea level (e.g. 35m or 115ft):', False);
  LocPage.Add('Antenna height above ground (e.g. 8m or 25ft, optional):', False);
  LocPage.Values[0] := Param('LAT', '');
  LocPage.Values[1] := Param('LON', '');
  LocPage.Values[2] := Param('ALT', '');
  LocPage.Values[3] := Param('AGL', '');
  NoLocation := TNewCheckBox.Create(LocPage);
  NoLocation.Parent := LocPage.Surface;
  NoLocation.Caption := 'Leave blank, let the network locate me';
  NoLocation.Top := LocPage.Edits[3].Top + LocPage.Edits[3].Height + ScaleY(10);
  NoLocation.Width := LocPage.SurfaceWidth;
  NoLocation.Checked := HasSwitch('NOLOCATION');

  CreatePrivacyPages(LocPage.ID);

  LanPage := CreateInputQueryPage(PinPage.ID, 'Status page on the network',
    'Choose a password for the status page',
    'Other computers on your network can then open http://<this computer>:8099/ and must log in with this password. The firewall rule is added for private networks only.');
  LanPage.Add('Password (at least 8 characters):', True);
  LanPage.Add('Repeat the password:', True);
end;

function ServerGiven: Boolean;
begin
  Result := Trim(AggPage.Values[0]) <> '';
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  Result := False;
  if (PageID = LocPage.ID) or (PageID = TsPage.ID) or (PageID = PinPage.ID) then
    Result := not ServerGiven;
  if PageID = LanPage.ID then
    Result := not WizardIsTaskSelected('lanui');
end;

{ SetupArgs is the "airview-feeder setup" command line for the values on the pages. }
function SetupArgs(const ConfigFile, Data: String): String;
var
  S: String;
begin
  S := 'setup -non-interactive -config ' + Q(ConfigFile) + ' -data ' + Q(Data) +
    ' -server ' + Q(Trim(AggPage.Values[0]));
  if Trim(AggPage.Values[1]) <> '' then
    S := S + ' -pin ' + Q(Trim(AggPage.Values[1]));
  if Trim(AggPage.Values[2]) <> '' then
    S := S + ' -enrol ' + Q(Trim(AggPage.Values[2]));
  if Trim(AggPage.Values[3]) <> '' then
    S := S + ' -name ' + Q(Trim(AggPage.Values[3]));
  if NoLocation.Checked then
    S := S + ' -no-location'
  else
  begin
    if Trim(LocPage.Values[0]) <> '' then
      S := S + ' -lat ' + Q(Trim(LocPage.Values[0]));
    if Trim(LocPage.Values[1]) <> '' then
      S := S + ' -lon ' + Q(Trim(LocPage.Values[1]));
    if Trim(LocPage.Values[2]) <> '' then
      S := S + ' -alt ' + Q(Trim(LocPage.Values[2]));
    if Trim(LocPage.Values[3]) <> '' then
      S := S + ' -agl ' + Q(Trim(LocPage.Values[3]));
  end;
  if TsNo.Checked then
    S := S + ' -no-timestamps'
  else
    S := S + ' -timestamps';
  if PinRegion.Checked then
    S := S + ' -privacy region'
  else if PinArea.Checked then
    S := S + ' -privacy cell01'
  else
    S := S + ' -privacy hidden';
  Result := S;
end;

{ ValidateSetup runs the real "setup" against a scratch configuration in
  {tmp}, so mistakes are reported while the user can still fix them. }
function ValidateSetup: Boolean;
var
  Exe, Dir, Output: String;
  Code: Integer;
begin
  Result := True;
  ExtractTemporaryFile('airview-feeder.exe');
  Exe := ExpandConstant('{tmp}\airview-feeder.exe');
  Dir := ExpandConstant('{tmp}\validate');
  ForceDirectories(Dir);
  Code := RunFeeder(Exe, SetupArgs(Dir + '\feeder.json', Dir), Output);
  DelTree(Dir, True, True, True);
  if Code <> 0 then
  begin
    SuppressibleMsgBox('These settings were not accepted:' + #13#10#13#10 + Output, mbError, MB_OK, IDOK);
    Result := False;
  end;
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  I: Integer;
begin
  Result := True;
  if CurPageID = AggPage.ID then
  begin
    for I := 0 to 3 do
      if not Valid(AggPage.Values[I]) then
      begin
        SuppressibleMsgBox('Quotes and line breaks are not allowed.', mbError, MB_OK, IDOK);
        Result := False;
        exit;
      end;
  end
  else if CurPageID = LocPage.ID then
  begin
    for I := 0 to 3 do
      if not Valid(LocPage.Values[I]) then
      begin
        SuppressibleMsgBox('Quotes and line breaks are not allowed.', mbError, MB_OK, IDOK);
        Result := False;
        exit;
      end;
    if (not NoLocation.Checked) and ((Trim(LocPage.Values[0]) = '') or (Trim(LocPage.Values[1]) = '') or (Trim(LocPage.Values[2]) = '')) then
    begin
      SuppressibleMsgBox('Enter latitude, longitude and altitude, or tick "Leave blank, let the network locate me".', mbError, MB_OK, IDOK);
      Result := False;
    end;
  end
  else if CurPageID = PinPage.ID then
    Result := ValidateSetup
  else if CurPageID = LanPage.ID then
  begin
    if Length(LanPage.Values[0]) < 8 then
    begin
      SuppressibleMsgBox('The password needs at least 8 characters.', mbError, MB_OK, IDOK);
      Result := False;
    end
    else if LanPage.Values[0] <> LanPage.Values[1] then
    begin
      SuppressibleMsgBox('The passwords differ.', mbError, MB_OK, IDOK);
      Result := False;
    end
    else if not Valid(LanPage.Values[0]) then
    begin
      SuppressibleMsgBox('Quotes and line breaks are not allowed in the password.', mbError, MB_OK, IDOK);
      Result := False;
    end;
  end;
end;

{ Before files are replaced: close the trays of this session and stop the
  service, so the programs are not in use. }
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Output: String;
begin
  Result := '';
  if FileExists(FeederExe) then
  begin
    RunFeeder(FeederExe, 'tray -signal quit', Output);
    RunFeeder(FeederExe, 'service stop -name {#ServiceName} -wait 90s', Output);
  end;
  if WizardSilent and ServerGiven then
    if not ValidateSetup then
      Result := 'The setup values given on the command line were not accepted (see the setup log).';
end;

procedure ReportFailure(const What, Output: String);
begin
  SuppressibleMsgBox(What + #13#10#13#10 + Output + #13#10 + 'You can finish this later: see README.txt in ' + ExpandConstant('{app}') + '.', mbError, MB_OK, IDOK);
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  Output, Conf: String;
  Code: Integer;
begin
  if CurStep <> ssPostInstall then
    exit;
  Conf := DataDir + '\feeder.json';
  ForceDirectories(DataDir);

  if ServerGiven then
    if RunFeeder(FeederExe, SetupArgs(Conf, DataDir), Output) <> 0 then
      ReportFailure('The feeder configuration could not be written.', Output);

  { Signed updates: where the network publishes them and its release key
    (like the Pi installer's --manifest-key; base64, 44 characters). }
  if (Param('MANIFESTURL', '') <> '') and (Length(Param('MANIFESTKEY', '')) = 44) then
    if RunFeeder(FeederExe, 'config set -config ' + Q(Conf) + ' -data ' + Q(DataDir) +
      ' ' + Q('update.manifestUrl=' + Param('MANIFESTURL', '')) + ' ' + Q('update.keys=' + Param('MANIFESTKEY', '')), Output) <> 0 then
      ReportFailure('The update settings could not be written.', Output);

  if WizardIsTaskSelected('lanui') and (LanPage.Values[0] <> '') then
  begin
    Redact := LanPage.Values[0];
    if RunFeeder(FeederExe, 'config set -config ' + Q(Conf) + ' -data ' + Q(DataDir) +
      ' ' + Q('ui.passwordHash=' + LanPage.Values[0]) + ' ui.addr=0.0.0.0:8099', Output) <> 0 then
      ReportFailure('The status page could not be opened to the network.', Output)
    else
    begin
      Exec(ExpandConstant('{sys}\netsh.exe'), 'advfirewall firewall delete rule name="{#FirewallRule}"', '', SW_HIDE, ewWaitUntilTerminated, Code);
      if not Exec(ExpandConstant('{sys}\netsh.exe'),
        'advfirewall firewall add rule name="{#FirewallRule}" dir=in action=allow protocol=TCP localport=8099 profile=private,domain program=' + Q(FeederExe),
        '', SW_HIDE, ewWaitUntilTerminated, Code) or (Code <> 0) then
        Log('netsh: could not add the firewall rule');
    end;
  end;

  if WizardIsTaskSelected('service') then
  begin
    if RunFeeder(FeederExe, 'service install -name {#ServiceName} -now', Output) <> 0 then
      ReportFailure('The Windows service could not be installed or started.', Output);
  end
  else
    { No service: the tray runs the feeder for the logged-on user. A service
      left from an earlier install would hold the identity, so remove it. }
    RunFeeder(FeederExe, 'service remove -name {#ServiceName}', Output);

  { Daily signed update as SYSTEM at a random time between 01:00 and 05:59
    (spreads the load over the network's feeders). "update" exits 3 and
    changes nothing until update.manifestUrl / update.keys are configured. }
  Exec(ExpandConstant('{sys}\schtasks.exe'), '/Delete /TN "{#UpdateTask}" /F', '', SW_HIDE, ewWaitUntilTerminated, Code);
  if WizardIsTaskSelected('service') and WizardIsTaskSelected('autoupdate') then
    if not Exec(ExpandConstant('{sys}\schtasks.exe'),
      '/Create /F /TN "{#UpdateTask}" /SC DAILY /ST ' + Format('%.2d:%.2d', [1 + Random(5), Random(60)]) +
      ' /RU SYSTEM /RL HIGHEST /TR "\"' + FeederExe + '\" update -service {#ServiceName}"',
      '', SW_HIDE, ewWaitUntilTerminated, Code) or (Code <> 0) then
      Log('schtasks: could not create the update task');
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  Output: String;
  Code: Integer;
begin
  if CurUninstallStep = usUninstall then
  begin
    RunFeeder(FeederExe, 'tray -signal quit', Output);
    RunFeeder(FeederExe, 'service remove -name {#ServiceName}', Output);
    Exec(ExpandConstant('{sys}\netsh.exe'), 'advfirewall firewall delete rule name="{#FirewallRule}"', '', SW_HIDE, ewWaitUntilTerminated, Code);
    Exec(ExpandConstant('{sys}\schtasks.exe'), '/Delete /TN "{#UpdateTask}" /F', '', SW_HIDE, ewWaitUntilTerminated, Code);
    RegDeleteValue(HKCU, 'Software\Microsoft\Windows\CurrentVersion\Run', '{#TrayRunValue}');
    if (not UninstallSilent) and DirExists(DataDir) then
      if MsgBox('Also delete the feeder''s configuration, identity and logs in ' + DataDir + '?' + #13#10#13#10 +
        'Keep them (No) if you will reinstall: the station keeps its identity and its claim. Deleted, the feeder becomes a new station.',
        mbConfirmation, MB_YESNO or MB_DEFBUTTON2) = IDYES then
      begin
        RunFeeder(FeederExe, 'uninstall-helper -data ' + Q(DataDir) + ' -purge -yes', Output);
        DelTree(DataDir + '\logs', True, True, True);
        RemoveDir(DataDir);
      end;
  end;
end;
