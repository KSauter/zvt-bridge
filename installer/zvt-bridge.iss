; Inno Setup 7 script for the Windows installer of zvt-bridge.
;
; Build: installer\build.ps1 (downloads usbip-win2, checks its SHA-256, calls ISCC)
;   or by hand: iscc /DAppVersion=1.2.3 /DBinDir=<dir with zvt-bridge.exe and USBip-*.exe> zvt-bridge.iss
;
; The installer owns the program files, the Start menu entries and the
; Settings > Apps entry. Service, usbip-win2 and the COM port are set up by
; "zvt-bridge.exe setup -installer", the same code the console setup uses.
;
; Unattended:
;   zvt-bridge-setup_<v>_windows_amd64.exe /VERYSILENT /TERMINAL=192.168.10.50[:20007] [/COM=COM8]
; Without /TERMINAL a silent update keeps the configured terminal.

#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif
#ifndef FileVersion
  #define FileVersion "0.0.0.0"
#endif
#ifndef BinDir
  #define BinDir "..\dist\installer"
#endif
#ifndef UsbipVersion
  #define UsbipVersion "0.9.8.1"
#endif
#define UsbipFile "USBip-" + UsbipVersion + "-x64.exe"

[Setup]
; Never change the AppId: updates find the installed version by it.
AppId={{92133E80-084A-469D-B923-E3B7271C8CFA}
AppName=ZVT Bridge
AppVersion={#AppVersion}
AppVerName=ZVT Bridge {#AppVersion}
AppPublisher=Kevin Sauter
AppPublisherURL=https://github.com/KSauter/zvt-bridge
AppSupportURL=https://github.com/KSauter/zvt-bridge/issues
VersionInfoVersion={#FileVersion}
VersionInfoProductName=ZVT Bridge
VersionInfoDescription=ZVT Bridge Setup
DefaultDirName={autopf}\zvt-bridge
DisableDirPage=yes
DefaultGroupName=ZVT Bridge
DisableProgramGroupPage=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
OutputDir=..\dist
OutputBaseFilename=zvt-bridge-setup_{#AppVersion}_windows_amd64
SetupIconFile=..\winres\icon.ico
UninstallDisplayIcon={app}\zvt-bridge.exe
UninstallDisplayName=ZVT Bridge
LicenseFile=..\LICENSE
WizardStyle=modern
Compression=lzma2
SolidCompression=yes
; The service is stopped in PrepareToInstall; Restart Manager would only
; ask the user about it.
CloseApplications=no
SetupLogging=yes

[Languages]
Name: "de"; MessagesFile: "compiler:Languages\German.isl"
Name: "en"; MessagesFile: "compiler:Default.isl"

[CustomMessages]
de.PageCaption=Zahlungsterminal und COM-Port
de.PageDescription=Wie erreicht die Bridge das EC-Terminal?
de.PageSub=Das Terminal muss auf ZVT über TCP/IP eingestellt sein ("ECR via LAN"). Den COM-Port für die Kassensoftware vergibt Windows automatisch; eine feste Nummer (z. B. COM8) macht mehrere Kassen einheitlich.
de.FieldHost=IP-Adresse oder Hostname des Terminals:
de.FieldPort=TCP-Port für ZVT (meist 20007):
de.FieldCom=COM-Port für die Kassensoftware (leer = automatisch):
de.ErrHost=Bitte die IP-Adresse oder den Hostnamen des Terminals angeben (nur Buchstaben, Ziffern, Punkt und Bindestrich).
de.ErrPort=Der Port muss eine Zahl zwischen 1 und 65535 sein.
de.ErrCom=Der COM-Port muss leer bleiben oder z. B. COM8 lauten.
de.ErrNoTerminal=Kein Terminal angegeben. Bei einer stillen Erstinstallation /TERMINAL=<IP>[:Port] übergeben.
de.StatusConfigure=Dienst, USB-Treiber und COM-Port werden eingerichtet ...
de.StatusStop=Laufender Dienst wird angehalten ...
de.SetupFailed=Die Einrichtung ist fehlgeschlagen:%n%n%1%n%nDas vollständige Protokoll liegt unter%n%2%n%nNach einem Neustart von Windows hilft meist "COM-Port reparieren" im Startmenü (ZVT Bridge).
de.FinishedCom=Die Kassensoftware nutzt %1 (9600 Baud, 8 Datenbits, keine Parität, 2 Stoppbits).
de.ShortcutRepair=COM-Port reparieren
de.ShortcutLog=Protokoll anzeigen
en.PageCaption=Payment terminal and COM port
en.PageDescription=How does the bridge reach the payment terminal?
en.PageSub=The terminal must be set to ZVT over TCP/IP ("ECR via LAN"). Windows assigns the COM port for the cash register software; a fixed number (e.g. COM8) makes several tills uniform.
en.FieldHost=IP address or host name of the terminal:
en.FieldPort=TCP port for ZVT (usually 20007):
en.FieldCom=COM port for the cash register software (empty = automatic):
en.ErrHost=Please enter the IP address or host name of the terminal (letters, digits, dot and hyphen only).
en.ErrPort=The port must be a number from 1 to 65535.
en.ErrCom=Leave the COM port empty or enter e.g. COM8.
en.ErrNoTerminal=No terminal given. Pass /TERMINAL=<ip>[:port] for a silent first installation.
en.StatusConfigure=Setting up the service, the USB driver and the COM port ...
en.StatusStop=Stopping the running service ...
en.SetupFailed=The setup failed:%n%n%1%n%nThe full log is in%n%2%n%nAfter a restart of Windows, "Repair COM port" in the Start menu (ZVT Bridge) usually helps.
en.FinishedCom=The cash register software uses %1 (9600 baud, 8 data bits, no parity, 2 stop bits).
en.ShortcutRepair=Repair COM port
en.ShortcutLog=Show log

[Files]
Source: "{#BinDir}\zvt-bridge.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\LICENSE"; DestDir: "{app}"; DestName: "LICENSE.txt"
Source: "usbip-win2-LICENSE.txt"; DestDir: "{app}"
Source: "{#BinDir}\{#UsbipFile}"; DestDir: "{tmp}"; Flags: deleteafterinstall

[Icons]
Name: "{group}\{cm:ShortcutRepair}"; Filename: "{app}\zvt-bridge.exe"; Parameters: "repair"
Name: "{group}\{cm:ShortcutLog}"; Filename: "{sys}\notepad.exe"; Parameters: """{commonappdata}\zvt-bridge\zvt-bridge.log"""
Name: "{group}\{cm:UninstallProgram,ZVT Bridge}"; Filename: "{uninstallexe}"

[UninstallRun]
; Removes the service (which unplugs the device) before the files go.
Filename: "{app}\zvt-bridge.exe"; Parameters: "uninstall -installer -yes"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveService"

[Code]
var
  Page: TInputQueryWizardPage;
  ComResult: AnsiString;

function ConfigFile: String;
begin
  Result := ExpandConstant('{commonappdata}\zvt-bridge\zvt-bridge.conf');
end;

function SetupLogFile: String;
begin
  Result := ExpandConstant('{commonappdata}\zvt-bridge\setup.log');
end;

{ "terminal = host:port" from the configuration of an installed version. }
function ConfiguredTerminal: String;
var
  Lines: TArrayOfString;
  I, P: Integer;
  Line: String;
begin
  Result := '';
  if not LoadStringsFromFile(ConfigFile, Lines) then
    Exit;
  for I := 0 to GetArrayLength(Lines) - 1 do
  begin
    Line := Trim(Lines[I]);
    P := Pos('=', Line);
    if (P > 0) and (CompareText(Trim(Copy(Line, 1, P - 1)), 'terminal') = 0) then
      Result := Trim(Copy(Line, P + 1, Length(Line)));
  end;
end;

{ Splits host[:port]; an address with several colons (IPv6) is left whole. }
procedure SplitHostPort(Addr: String; var Host, Port: String);
var
  P: Integer;
begin
  Host := Addr;
  Port := '';
  P := Pos(':', Addr);
  if (P > 0) and (Pos(':', Copy(Addr, P + 1, Length(Addr))) = 0) then
  begin
    Host := Copy(Addr, 1, P - 1);
    Port := Copy(Addr, P + 1, Length(Addr));
  end;
end;

function ValidPort(S: String): Boolean;
var
  N: Integer;
begin
  N := StrToIntDef(S, 0);
  Result := (N >= 1) and (N <= 65535);
end;

{ Host names and IPv4 addresses only: the value ends up on a command line. }
function ValidHost(S: String): Boolean;
var
  I: Integer;
begin
  Result := (S <> '') and (Length(S) <= 253);
  for I := 1 to Length(S) do
    if Pos(S[I], 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-') = 0 then
      Result := False;
end;

function ValidCom(S: String): Boolean;
var
  N: Integer;
begin
  Result := (S = '') or ((Length(S) > 3) and (CompareText(Copy(S, 1, 3), 'COM') = 0));
  if Result and (S <> '') then
  begin
    N := StrToIntDef(Copy(S, 4, Length(S)), 0);
    Result := (N >= 1) and (N <= 256);
  end;
end;

procedure InitializeWizard;
var
  Host, Port, Com: String;
begin
  Page := CreateInputQueryPage(wpLicense, CustomMessage('PageCaption'),
    CustomMessage('PageDescription'), CustomMessage('PageSub'));
  Page.Add(CustomMessage('FieldHost'), False);
  Page.Add(CustomMessage('FieldPort'), False);
  Page.Add(CustomMessage('FieldCom'), False);

  { Command line first, then what the installed version uses. }
  SplitHostPort(ExpandConstant('{param:TERMINAL|}'), Host, Port);
  if Host = '' then
    SplitHostPort(ConfiguredTerminal, Host, Port);
  if Port = '' then
    Port := GetPreviousData('TerminalPort', '20007');
  Com := ExpandConstant('{param:COM|}');
  if Com = '' then
    Com := GetPreviousData('ComPort', '');
  Page.Values[0] := Host;
  Page.Values[1] := Port;
  Page.Values[2] := Com;
end;

function ValidateInput(var Msg: String): Boolean;
var
  Host, Port: String;
begin
  { "192.168.10.50:5577" typed into the host field }
  SplitHostPort(Trim(Page.Values[0]), Host, Port);
  Page.Values[0] := Host;
  if Port <> '' then
    Page.Values[1] := Port;
  Page.Values[1] := Trim(Page.Values[1]);
  Page.Values[2] := Uppercase(Trim(Page.Values[2]));
  if CompareText(Page.Values[2], 'AUTO') = 0 then
    Page.Values[2] := '';
  Result := False;
  if not ValidHost(Page.Values[0]) then
    Msg := CustomMessage('ErrHost')
  else if not ValidPort(Page.Values[1]) then
    Msg := CustomMessage('ErrPort')
  else if not ValidCom(Page.Values[2]) then
    Msg := CustomMessage('ErrCom')
  else
    Result := True;
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  Msg: String;
begin
  Result := True;
  if CurPageID = Page.ID then
  begin
    Result := ValidateInput(Msg);
    if not Result then
      MsgBox(Msg, mbError, MB_OK);
  end;
end;

procedure RegisterPreviousData(PreviousDataKey: Integer);
begin
  SetPreviousData(PreviousDataKey, 'TerminalPort', Page.Values[1]);
  SetPreviousData(PreviousDataKey, 'ComPort', Page.Values[2]);
end;

{ Runs before any file is copied: checks the input (also for /VERYSILENT,
  where no page is shown) and stops the service so zvt-bridge.exe can be
  replaced. This also covers the self-installed versions 1.0.x, which used
  the same directory. }
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Msg: String;
  Code: Integer;
  Exe: String;
begin
  Result := '';
  if Page.Values[0] = '' then
  begin
    Result := CustomMessage('ErrNoTerminal');
    Exit;
  end;
  if not ValidateInput(Msg) then
  begin
    Result := Msg;
    Exit;
  end;
  WizardForm.PreparingLabel.Caption := CustomMessage('StatusStop');
  Exe := ExpandConstant('{app}\zvt-bridge.exe');
  if FileExists(Exe) then
    Exec(Exe, 'stop', '', SW_HIDE, ewWaitUntilTerminated, Code)
  else
    Exec(ExpandConstant('{sys}\sc.exe'), 'stop ZvtBridge', '', SW_HIDE, ewWaitUntilTerminated, Code);
end;

{ The last lines of the setup log, for the error message. }
function LogTail(Path: String; Count: Integer): String;
var
  Lines: TArrayOfString;
  I, First: Integer;
begin
  Result := '';
  if not LoadStringsFromFile(Path, Lines) then
    Exit;
  First := GetArrayLength(Lines) - Count;
  if First < 0 then
    First := 0;
  for I := First to GetArrayLength(Lines) - 1 do
    if Trim(Lines[I]) <> '' then
      Result := Result + Lines[I] + #13#10;
end;

procedure ConfigureService;
var
  Params, ResultFile: String;
  Code: Integer;
begin
  WizardForm.StatusLabel.Caption := CustomMessage('StatusConfigure');
  WizardForm.ProgressGauge.Style := npbstMarquee;
  ResultFile := ExpandConstant('{tmp}\com.txt');
  { The mode is not passed: an update keeps a configuration that uses an
    existing COM port instead of the emulated one. }
  Params := 'setup -installer -yes' +
    ' -terminal "' + Page.Values[0] + ':' + Page.Values[1] + '"' +
    ' -usbip-setup "' + ExpandConstant('{tmp}\{#UsbipFile}') + '"' +
    ' -result "' + ResultFile + '"' +
    ' -output "' + SetupLogFile + '"';
  if Page.Values[2] <> '' then
    Params := Params + ' -com ' + Page.Values[2];
  Log('zvt-bridge.exe ' + Params);
  { Started directly, no cmd.exe: zvt-bridge.exe writes its output to
    SetupLogFile itself (-output). }
  if not Exec(ExpandConstant('{app}\zvt-bridge.exe'), Params, '', SW_HIDE,
      ewWaitUntilTerminated, Code) then
    Code := -1;
  WizardForm.ProgressGauge.Style := npbstNormal;
  Log(LogTail(SetupLogFile, 200));
  if Code <> 0 then
  begin
    SuppressibleMsgBox(FmtMessage(CustomMessage('SetupFailed'), [LogTail(SetupLogFile, 12),
      SetupLogFile]), mbError, MB_OK, IDOK);
    Exit;
  end;
  if not LoadStringFromFile(ResultFile, ComResult) then
    ComResult := '';
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssPostInstall then
    ConfigureService;
end;

procedure CurPageChanged(CurPageID: Integer);
begin
  if (CurPageID = wpFinished) and (ComResult <> '') then
    WizardForm.FinishedLabel.Caption := WizardForm.FinishedLabel.Caption + #13#10#13#10 +
      FmtMessage(CustomMessage('FinishedCom'), [String(ComResult)]);
end;
