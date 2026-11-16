; The Windows installer of the native Dilla desktop app (GPUI release plan, Task 13, decision D10).
;
; Built by scripts/desktop-gpui-package-win.ps1 with Inno Setup 6, which passes the defines below:
;   /DAppVersion=0.2.0-alpha.1   the release version        /DNumericVersion=0.2.0.1   its x.x.x.x form (file properties)
;   /DStageDir=<dir>             holds dilla-desktop.exe, dilla.ico and licenses\
;   /DOutDir=<dir>               where dilla-<v>-win-x64-setup.exe goes
;   /DWithSigning                only when a signtool command is configured (a disabled hook until signing is decided)
;
; Install: per user (no elevation) to %LOCALAPPDATA%\Programs\Dilla. beta.5 (one-click NSIS) used %LOCALAPPDATA%\Programs\@dilladesktop;
; the takeover below finds it through its registry entries, never by path.
; The Start-menu shortcut carries AppUserModelID dev.dilla.desktop, which Windows needs to show the app's toasts
; (dilla-desktop's APP_ID, src/native/notifier.rs). The dilla:// protocol is registered per user
; ([Registry] below; D12 superseded 2026-11-16) and removed with the app.
; Silent: `setup.exe /VERYSILENT /SUPPRESSMSGBOXES` installs and `unins000.exe /VERYSILENT /SUPPRESSMSGBOXES` removes
; it; the updater (Task 14) uses the same flags.
; User data (%APPDATA%\Dilla from beta.5, `Dilla Preview` for the GPUI app) is never touched, by install or uninstall.

#ifndef AppVersion
  #error AppVersion is not defined (pass /DAppVersion=<version>)
#endif
#ifndef NumericVersion
  #error NumericVersion is not defined (pass /DNumericVersion=<x.x.x.x>)
#endif
#ifndef StageDir
  #error StageDir is not defined (pass /DStageDir=<dir>)
#endif
#ifndef OutDir
  #error OutDir is not defined (pass /DOutDir=<dir>)
#endif

#define AppId "dev.dilla.desktop"
#define Exe "dilla-desktop.exe"

[Setup]
; Inno's own identity, not beta.5's. The brace is doubled ({{) because {x} is a constant in Inno.
AppId={{6F0A3C1E-5D2B-4B7A-9E43-1C8D2A7B5F10}
AppName=Dilla
AppVersion={#AppVersion}
AppVerName=Dilla {#AppVersion}
AppPublisher=Dilla
AppPublisherURL=https://github.com/dilla-chat/dilla
VersionInfoVersion={#NumericVersion}
VersionInfoProductName=Dilla
VersionInfoDescription=Dilla setup
DefaultDirName={localappdata}\Programs\Dilla
DisableDirPage=yes
DisableProgramGroupPage=yes
DisableReadyPage=yes
DisableWelcomePage=yes
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
OutputDir={#OutDir}
OutputBaseFilename=dilla-{#AppVersion}-win-x64-setup
SetupIconFile={#StageDir}\dilla.ico
UninstallDisplayIcon={app}\{#Exe}
UninstallDisplayName=Dilla
WizardStyle=modern
Compression=lzma2/ultra64
SolidCompression=yes
; A running Dilla is closed (Restart Manager) before its files are replaced, and not started again by Setup itself.
CloseApplications=yes
RestartApplications=no
#ifdef WithSigning
; The disabled signtool hook (D10): scripts/desktop-gpui-package-win.ps1 defines the `dilla_sign` command with iscc's
; /S option only when DILLA_SIGNTOOL_COMMAND is set. Nothing sets it for the alpha.
SignTool=dilla_sign
SignedUninstaller=yes
#endif

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Files]
Source: "{#StageDir}\{#Exe}"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#StageDir}\licenses\*"; DestDir: "{app}\licenses"; Flags: ignoreversion recursesubdirs

[Icons]
; The AppUserModelID must equal the app's (dilla-desktop's APP_ID): toasts are attributed to it.
Name: "{userprograms}\Dilla"; Filename: "{app}\{#Exe}"; AppUserModelID: "{#AppId}"

[Registry]
; The app's own "start at login" value (src/native/autostart.rs); removed with the app, nothing created.
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: none; ValueName: "Dilla"; Flags: dontcreatekey uninsdeletevalue

; The dilla:// scheme (beta.5's electron-builder protocols): the app takes the link as its one argument, a second launch
; hands it to the running copy (src/native/single_instance.rs). Per user, so no elevation; removed on uninstall.
Root: HKCU; Subkey: "Software\Classes\dilla"; ValueType: string; ValueName: ""; ValueData: "URL:Dilla"; Flags: uninsdeletekey
Root: HKCU; Subkey: "Software\Classes\dilla"; ValueType: string; ValueName: "URL Protocol"; ValueData: ""
Root: HKCU; Subkey: "Software\Classes\dilla\DefaultIcon"; ValueType: string; ValueName: ""; ValueData: "{app}\{#Exe},0"
Root: HKCU; Subkey: "Software\Classes\dilla\shell\open\command"; ValueType: string; ValueName: ""; ValueData: """{app}\{#Exe}"" ""%1"""

[Run]
Filename: "{app}\{#Exe}"; Description: "Start Dilla"; Flags: nowait postinstall skipifsilent
; The in-app updater (src/updater/apply_win.rs) runs Setup silently with /relaunch=1 while the app quits: start the new
; app with --updated, which waits for the quitting copy's single-instance lock (src/native/single_instance.rs).
Filename: "{app}\{#Exe}"; Parameters: "--updated"; Flags: nowait runasoriginaluser; Check: RelaunchRequested

[UninstallDelete]
; Nothing else lives in the program folder; a leftover file must not keep it on disk.
Type: filesandordirs; Name: "{app}"

[Code]
// The beta.5 takeover. beta.5's Electron app was installed by electron-builder's one-click, per-user NSIS installer:
//   HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\<APP_GUID>   UninstallString, QuietUninstallString
//   HKCU\Software\<APP_GUID>                                              InstallLocation
// where APP_GUID is electron-builder's UUID v5 of the appId "dev.dilla.desktop" in its fixed namespace
// 50e065bc-3134-11e6-9bab-38c9862bdaf3 (app-builder-lib NsisTarget.js; scripts/desktop-gpui-package-win.test.mjs
// recomputes it). The uninstaller runs as electron-builder's own installer runs an old one on an update
// (installUtil.nsh, uninstallOldVersion): `/S /KEEP_APP_DATA /currentuser --updated _?=<install dir>`, which waits
// for it and keeps %APPDATA%\Dilla. It lives in the folder this installer writes to, so it goes before any file is copied.
function RelaunchRequested(): Boolean;
begin
  Result := ExpandConstant('{param:relaunch|0}') = '1';
end;

const
  Beta5Guid = 'f6db60e4-10df-5b69-9206-408d5f2cebe9';
  UninstallRoot = 'Software\Microsoft\Windows\CurrentVersion\Uninstall\';

function Beta5UninstallExe(): String;
var
  Command: String;
  Quote: Integer;
begin
  Result := '';
  if not RegQueryStringValue(HKCU, UninstallRoot + Beta5Guid, 'UninstallString', Command) then Exit;
  // "<dir>\Uninstall Dilla.exe" /currentuser: the program is the quoted part.
  Command := Trim(Command);
  if (Length(Command) > 1) and (Command[1] = '"') then
  begin
    Delete(Command, 1, 1);
    Quote := Pos('"', Command);
    if Quote > 0 then Result := Copy(Command, 1, Quote - 1);
  end
  else
    Result := Command;
end;

function UninstallBeta5(): String;
var
  Exe, Dir, Temp: String;
  Code, Attempt: Integer;
  Ran: Boolean;
begin
  Result := '';
  Exe := Beta5UninstallExe();
  if Exe = '' then Exit;  // beta.5 is not installed
  if not RegQueryStringValue(HKCU, 'Software\' + Beta5Guid, 'InstallLocation', Dir) then Dir := ExtractFileDir(Exe);
  Dir := RemoveBackslash(Dir);
  // Run a copy, as electron-builder does: the uninstaller is inside the folder it removes.
  Temp := ExpandConstant('{tmp}\dilla-beta5-uninstall.exe');
  if not FileCopy(Exe, Temp, False) then Temp := Exe;
  Code := -1;
  Ran := False;
  for Attempt := 1 to 5 do
  begin
    Ran := Exec(Temp, '/S /KEEP_APP_DATA /currentuser --updated _?=' + Dir, '', SW_HIDE, ewWaitUntilTerminated, Code);
    if Ran and (Code = 0) then Break;
    Sleep(1000);
  end;
  if not (Ran and (Code = 0)) then
  begin
    Result := 'Setup could not remove the earlier Dilla (its uninstaller ' + Exe + ' ended with code ' + IntToStr(Code) + '). Close Dilla and run Setup again.';
    Exit;
  end;
  // With `_?=` the uninstaller leaves itself behind; the folder is this installer's own, so clear it of the old app.
  DeleteFile(Exe);
  RemoveDir(Dir);
  if RegKeyExists(HKCU, UninstallRoot + Beta5Guid) then
    Result := 'Setup removed the earlier Dilla but its uninstall entry is still listed.';
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := UninstallBeta5();
end;
