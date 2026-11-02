; Full custom script: do not reintroduce the stock install/uninstall sections.
; APP_64 and its companion come only from the post-sign build hook.
!include "LogicLib.nsh"
!include "FileFunc.nsh"
!include "x64.nsh"
!include "MUI2.nsh"
!include "${APP_64}.bootstrap.nsh"
!define DILLA_UNINSTALL_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${UNINSTALL_APP_KEY}"
!ifndef DILLA_PROTOCOL_SCHEME
  !define DILLA_PROTOCOL_SCHEME "dilla"
!endif
!define DILLA_PROTOCOL_KEY "Software\Classes\${DILLA_PROTOCOL_SCHEME}"
!define DILLA_RUN_KEY "Software\Microsoft\Windows\CurrentVersion\Run"
Name "${PRODUCT_NAME}"
BrandingText "${PRODUCT_NAME} ${VERSION}"
RequestExecutionLevel user
ShowInstDetails show
ShowUninstDetails show
!ifdef MUI_ICON
  Icon "${MUI_ICON}"
  UninstallIcon "${MUI_ICON}"
!endif
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!ifmacrodef addLangs
  !insertmacro addLangs
!else
  !insertmacro MUI_LANGUAGE "English"
!endif
Var DataRoot
Var Staging
Var Arguments
Var OperationMutex
Var DataArchive
Var ProgramArchive
Var InitialRetry
Var GeneratedGUID
Var InitialMarkerArchived
Var StableLauncherHandle
Var ArchivedUpdatesExisting

Function Fail
  Call ReleaseStableLauncher
  Call RestoreInitialArchives
  Call ReleaseOperation
  MessageBox MB_OK|MB_ICONSTOP "Dilla setup could not safely complete. Quit Dilla from its tray menu and retry. Existing device data is preserved." /SD IDOK
  SetErrorLevel 2
  Quit
FunctionEnd
Function un.Fail
  Call un.ReleaseStableLauncher
  Call un.RestoreHistory
  Call un.ReleaseOperation
  MessageBox MB_OK|MB_ICONSTOP "Dilla could not safely uninstall. Quit Dilla from its tray menu and retry. Device data is preserved." /SD IDOK
  SetErrorLevel 2
  Quit
FunctionEnd

; Check the entire chain, including missing destinations' existing ancestors.
; Any access error other than missing file/path is a refusal, not a missing path.
!macro PathChecks Prefix
Function ${Prefix}CheckPath
  Exch $0
  Push $1
  Push $2
  Push $3
  loop:
    ; Capture the error before System plug-in cleanup can overwrite it.
    System::Call 'kernel32::GetFileAttributesW(w r0)i.r2 ?e'
    Pop $3
    ${If} $2 == -1
      ${If} $3 != 2
      ${AndIf} $3 != 3
        Call ${Prefix}Fail
      ${EndIf}
    ${Else}
      IntOp $1 $2 & 0x400
      ${If} $1 != 0
        Call ${Prefix}Fail
      ${EndIf}
    ${EndIf}
    ${GetParent} "$0" $1
    StrCmp $1 "" done
    StrCmp $1 $0 done
    StrCpy $0 $1
    Goto loop
  done:
  Pop $3
  Pop $2
  Pop $1
  Pop $0
FunctionEnd

Function ${Prefix}CheckOwnedRoot
  SetShellVarContext current
  SetRegView 64
  StrCpy $INSTDIR "$LOCALAPPDATA\Programs\${PRODUCT_NAME}"
  StrCpy $DataRoot "$APPDATA\${PRODUCT_NAME}\updates"
  Push "$INSTDIR"
  Call ${Prefix}CheckPath
  Push "$DataRoot"
  Call ${Prefix}CheckPath
  ReadRegStr $0 HKCU "${DILLA_UNINSTALL_KEY}" InstallLocation
  StrCmp $0 $INSTDIR +2
    Call ${Prefix}Fail
  Push "$INSTDIR\Dilla.exe"
  Call ${Prefix}CheckPath
  !if "${Prefix}" != "un."
    IfFileExists "$INSTDIR\Dilla.exe" +2
      Call ${Prefix}Fail
  !endif
FunctionEnd

; Before recursive deletion/copy, reject every nested reparse point.
Function ${Prefix}CheckTree
  Exch $R0
  Push $R1
  Push $R2
  Push $R3
  Push $R4
  Push "$R0"
  Call ${Prefix}CheckPath
  FindFirst $R1 $R2 "$R0\*"
  loop:
    StrCmp $R2 "" done
    StrCmp $R2 "." next
    StrCmp $R2 ".." next
    Push "$R0\$R2"
    Call ${Prefix}CheckPath
    System::Call 'kernel32::GetFileAttributesW(w "$R0\$R2")i.r0'
    IntOp $R3 $0 & 0x10
    ${If} $R3 != 0
      Push "$R0\$R2"
      Call ${Prefix}CheckTree
    ${Else}
      Push "$R0\$R2"
      Call ${Prefix}RequireUnlockedFile
    ${EndIf}
  next:
    FindNext $R1 $R2
    Goto loop
  done:
    FindClose $R1
    Pop $R4
    Pop $R3
    Pop $R2
    Pop $R1
    Pop $R0
FunctionEnd
!macroend
!insertmacro PathChecks ""
!insertmacro PathChecks "un."

Function .onInit
  StrCpy $OperationMutex 0
  StrCpy $StableLauncherHandle 0
  SetShellVarContext current
  SetRegView 64
  ${IfNot} ${RunningX64}
    Call Fail
  ${EndIf}
  ; Reset /D= before any operation. No argument has directory authority.
  StrCpy $INSTDIR "$LOCALAPPDATA\Programs\${PRODUCT_NAME}"
  StrCpy $DataRoot "$APPDATA\${PRODUCT_NAME}\updates"
  StrCpy $Staging 0
  ${GetParameters} $Arguments
  ClearErrors
  ${GetOptions} $Arguments "--updated" $0
  ${IfNot} ${Errors}
    StrCmp $0 "" +2
      Call Fail
    IfSilent +2
      Call Fail
    StrCpy $Staging 1
  ${EndIf}
  Call AcquireOperation
  InitPluginsDir
FunctionEnd

Function ExtractPayload
  ; Never extract the complete payload into the live program root.
  SetOutPath "$PLUGINSDIR\payload"
  File /oname=$PLUGINSDIR\payload.7z "${APP_64}"
  ClearErrors
  Nsis7z::Extract "$PLUGINSDIR\payload.7z"
  IfErrors 0 +2
    Call Fail
  IfFileExists "$PLUGINSDIR\payload\versions\${DILLA_PROGRAM_ID}\dilla-desktop.exe" +2
    Call Fail
  IfFileExists "$PLUGINSDIR\payload\manifests\${DILLA_MANIFEST_NAME}" +2
    Call Fail
  Push "$PLUGINSDIR\payload"
  Call CheckTree
FunctionEnd

Function RequireAbsentPath
  Exch $0
  Push $1
  Push "$0"
  Call CheckPath
  System::Call 'kernel32::GetFileAttributesW(w r0)i.r1'
  StrCmp $1 -1 +2
    Call Fail
  Pop $1
  Pop $0
FunctionEnd

Function CopyInactivePayload
  ; Existing (even partial) targets are immutable. Recovery handles quarantining.
  Push "$INSTDIR\versions"
  Call CheckPath
  Push "$INSTDIR\manifests"
  Call CheckPath
  Push "$INSTDIR\versions\${DILLA_PROGRAM_ID}"
  Call RequireAbsentPath
  Push "$INSTDIR\manifests\${DILLA_MANIFEST_NAME}"
  Call RequireAbsentPath
  IfFileExists "$INSTDIR\versions\${DILLA_PROGRAM_ID}\*" Fail
  IfFileExists "$INSTDIR\versions\${DILLA_PROGRAM_ID}" Fail
  IfFileExists "$INSTDIR\manifests\${DILLA_MANIFEST_NAME}" Fail
  ClearErrors
  CreateDirectory "$INSTDIR\versions\${DILLA_PROGRAM_ID}"
  CreateDirectory "$INSTDIR\manifests"
  CopyFiles /SILENT "$PLUGINSDIR\payload\versions\${DILLA_PROGRAM_ID}\*" "$INSTDIR\versions\${DILLA_PROGRAM_ID}"
  IfErrors Fail
  CopyFiles /SILENT "$PLUGINSDIR\payload\manifests\${DILLA_MANIFEST_NAME}" "$INSTDIR\manifests\${DILLA_MANIFEST_NAME}"
  IfErrors Fail
  Return
  Fail:
    Call Fail
FunctionEnd

Function StageInactiveVersion
  Call CheckOwnedRoot
  Call StagePreflight
  Call CopyInactivePayload
FunctionEnd

Function InstallInitial
  Call PrepareInitialInstallation
  ; A second interactive setup cannot reset accepted history or the launcher.
  Push "$INSTDIR"
  Call CheckPath
  Push "$DataRoot"
  Call CheckPath
  Push "$INSTDIR"
  Call RequireAbsentPath
  Push "$DataRoot"
  Call RequireAbsentPath
  ${If} $InitialRetry != 1
    IfFileExists "$SMPROGRAMS\${PRODUCT_NAME}.lnk" Fail
    IfFileExists "$DESKTOP\${PRODUCT_NAME}.lnk" Fail
  ${EndIf}
  IfFileExists "$INSTDIR\*" Fail
  IfFileExists "$DataRoot\*" Fail
  IfFileExists "$DataRoot\windows-active-0.json" Fail
  IfFileExists "$DataRoot\windows-active-1.json" Fail
  ReadRegStr $0 HKCU "${DILLA_UNINSTALL_KEY}" InstallLocation
  ${If} $0 != ""
    ${If} $InitialRetry != 1
      Goto Fail
    ${EndIf}
    StrCmp $0 $INSTDIR +2
      Goto Fail
  ${EndIf}
  Call CommitInitialMarker
  Call CopyInactivePayload
  ClearErrors
  CopyFiles /SILENT "$PLUGINSDIR\payload\Dilla.exe" "$INSTDIR\Dilla.exe"
  IfErrors Fail
  CreateDirectory "$DataRoot"
  CopyFiles /SILENT "$PLUGINSDIR\payload\initial-slot.json" "$DataRoot\windows-active-0.json"
  IfErrors Fail
  ; Flush the committed seed before any user entry point can launch it.
  System::Call 'kernel32::CreateFileW(w "$DataRoot\windows-active-0.json", i 0x40000000, i 0, p 0, i 3, i 0x80, p 0)p.r0'
  StrCmp $0 -1 Fail
  System::Call 'kernel32::FlushFileBuffers(p r0)i.r1'
  System::Call 'kernel32::CloseHandle(p r0)'
  StrCmp $1 0 Fail
  WriteUninstaller "$INSTDIR\Uninstall ${PRODUCT_NAME}.exe"
  IfErrors Fail
  ClearErrors
  WriteRegStr HKCU "${DILLA_UNINSTALL_KEY}" InstallLocation "$INSTDIR"
  WriteRegStr HKCU "${DILLA_UNINSTALL_KEY}" DisplayName "${PRODUCT_NAME}"
  WriteRegStr HKCU "${DILLA_UNINSTALL_KEY}" DisplayVersion "${VERSION}"
  WriteRegStr HKCU "${DILLA_UNINSTALL_KEY}" DisplayIcon '$\"$INSTDIR\Dilla.exe$\",0'
  WriteRegStr HKCU "${DILLA_UNINSTALL_KEY}" UninstallString '$\"$INSTDIR\Uninstall ${PRODUCT_NAME}.exe$\"'
  WriteRegStr HKCU "${DILLA_UNINSTALL_KEY}" QuietUninstallString '$\"$INSTDIR\Uninstall ${PRODUCT_NAME}.exe$\" /S'
  WriteRegDWORD HKCU "${DILLA_UNINSTALL_KEY}" NoModify 1
  WriteRegDWORD HKCU "${DILLA_UNINSTALL_KEY}" NoRepair 1
  WriteRegDWORD HKCU "${DILLA_UNINSTALL_KEY}" EstimatedSize ${ESTIMATED_SIZE}
  WriteRegStr HKCU "${DILLA_PROTOCOL_KEY}" "" "URL:Dilla invite"
  WriteRegStr HKCU "${DILLA_PROTOCOL_KEY}" "URL Protocol" ""
  WriteRegStr HKCU "${DILLA_PROTOCOL_KEY}\DefaultIcon" "" '$\"$INSTDIR\Dilla.exe$\",0'
  WriteRegStr HKCU "${DILLA_PROTOCOL_KEY}\shell\open\command" "" '$\"$INSTDIR\Dilla.exe$\" $\"%1$\"'
  IfFileExists "$SMPROGRAMS\${PRODUCT_NAME}.lnk" +2
    CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}.lnk" "$INSTDIR\Dilla.exe"
  IfFileExists "$DESKTOP\${PRODUCT_NAME}.lnk" +2
    CreateShortCut "$DESKTOP\${PRODUCT_NAME}.lnk" "$INSTDIR\Dilla.exe"
  IfErrors Fail
  ClearErrors
  Delete "$APPDATA\${PRODUCT_NAME}\initial-install.json"
  IfErrors Fail
  ; Installation never auto-launches. Ordinary shell entry runs the stable exe.
  Return
  Fail:
    Call Fail
FunctionEnd

Section "Install"
  ; Enforce fixed roots again after NSIS has processed all switches.
  StrCpy $INSTDIR "$LOCALAPPDATA\Programs\${PRODUCT_NAME}"
  StrCpy $DataRoot "$APPDATA\${PRODUCT_NAME}\updates"
  Call ExtractPayload
  ${If} $Staging == 1
    Call StageInactiveVersion
  ${Else}
    Call InstallInitial
  ${EndIf}
  Call ReleaseOperation
SectionEnd

; IShellLinkW/IPersistFile are Windows system COM interfaces. Delete only links
; that still resolve to our exact stable launcher with the arguments we created.
Function un.DeleteOwnedShortcut
  Exch $R0
  Push $0
  Push $1
  Push $2
  Push $3
  Push $4
  Push $5
  Push $6
  Push "$R0"
  Call un.CheckPath
  System::Call 'ole32::CoInitialize(p 0)i.r6'
  System::Call 'ole32::CoCreateInstance(g "{00021401-0000-0000-C000-000000000046}", p 0, i 1, g "{000214F9-0000-0000-C000-000000000046}", *p.r1)i.r2'
  ${If} $2 == 0
    System::Call '$1->0(g "{0000010b-0000-0000-C000-000000000046}", *p.r3)i.r2'
    ${If} $2 == 0
      System::Call '$3->5(w "$R0", i 0)i.r2'
      ${If} $2 == 0
        System::Call '$1->3(w .r4, i ${NSIS_MAX_STRLEN}, p 0, i 0)i.r2'
        ${If} $2 == 0
        ${AndIf} $4 == "$INSTDIR\Dilla.exe"
          System::Call '$1->10(w .r5, i ${NSIS_MAX_STRLEN})i.r2'
          ${If} $2 == 0
          ${AndIf} $5 == ""
            Delete "$R0"
          ${EndIf}
        ${EndIf}
      ${EndIf}
      System::Call '$3->2()'
    ${EndIf}
    System::Call '$1->2()'
  ${EndIf}
  ${If} $6 == 0
  ${OrIf} $6 == 1
    System::Call 'ole32::CoUninitialize()'
  ${EndIf}
  Pop $6
  Pop $5
  Pop $4
  Pop $3
  Pop $2
  Pop $1
  Pop $0
  Pop $R0
FunctionEnd

; FILE_WRITE_DATA (via GENERIC_WRITE) causes the kernel to reject an image
; that cannot be unmapped. DELETE access alone does not prove that condition.
; This is a preflight, not a retained process/race exclusion guarantee.
Function RequireUnlockedFile
  Exch $0
  Push $1
  Push "$0"
  Call CheckPath
  System::Call 'kernel32::CreateFileW(w r0, i 0x40010000, i 0, p 0, i 3, i 0x80, p 0)p.r1'
  StrCmp $1 -1 0 +2
    Call Fail
  System::Call 'kernel32::CloseHandle(p r1)'
  Pop $1
  Pop $0
FunctionEnd

Function un.RequireUnlockedFile
  Exch $0
  Push $1
  Push "$0"
  Call un.CheckPath
  System::Call 'kernel32::CreateFileW(w r0, i 0x40010000, i 0, p 0, i 3, i 0x80, p 0)p.r1'
  StrCmp $1 -1 0 +2
    Call un.Fail
  System::Call 'kernel32::CloseHandle(p r1)'
  Pop $1
  Pop $0
FunctionEnd

Function un.onInit
  StrCpy $StableLauncherHandle 0
  StrCpy $OperationMutex 0
  Call un.AcquireOperation
  InitPluginsDir
  Call un.CheckOwnedRoot
  ; NSIS copies the uninstaller to TEMP. $INSTDIR remains its originating root.
  ; Do not use $EXEDIR (the temporary copy) as uninstall authority.
FunctionEnd

Section "Uninstall"
  Call un.CheckOwnedRoot
  IfFileExists "$INSTDIR\Dilla.exe" 0 +3
    Push "$INSTDIR\Dilla.exe"
    Call un.RequireUnlockedFile
  Call un.MaintenancePreflight
  Push "$INSTDIR\versions"
  Call un.CheckTree
  Push "$INSTDIR\manifests"
  Call un.CheckTree
  ; A running parent could publish a new nonce and exit while the image scan
  ; runs. Revalidate after all images are unmapped, before any namespace moves.
  Call un.MaintenancePreflight
  ; Retain write/delete access with only delete sharing. New CreateProcess image
  ; mappings need read access and cannot race the final launcher deletion.
  IfFileExists "$INSTDIR\Dilla.exe" 0 +2
    Call un.HoldStableLauncher
  ; Archive only updater/installer metadata after every owned-image preflight.
  Call un.ArchiveHistory
  ClearErrors
  RMDir /r "$INSTDIR\versions"
  IfErrors Fail
  RMDir /r "$INSTDIR\manifests"
  IfErrors Fail
  IfFileExists "$INSTDIR\Dilla.exe" 0 +3
    Delete "$INSTDIR\Dilla.exe"
    IfErrors Fail
  Call un.ReleaseStableLauncher
  ReadRegStr $0 HKCU "${DILLA_PROTOCOL_KEY}\shell\open\command" ""
  ${If} $0 == '$\"$INSTDIR\Dilla.exe$\" $\"%1$\"'
    DeleteRegKey HKCU "${DILLA_PROTOCOL_KEY}"
  ${EndIf}
  ReadRegStr $0 HKCU "${DILLA_RUN_KEY}" "${PRODUCT_NAME}"
  ${If} $0 == '$\"$INSTDIR\Dilla.exe$\" --hidden'
    DeleteRegValue HKCU "${DILLA_RUN_KEY}" "${PRODUCT_NAME}"
  ${EndIf}
  Push "$SMPROGRAMS\${PRODUCT_NAME}.lnk"
  Call un.DeleteOwnedShortcut
  Push "$DESKTOP\${PRODUCT_NAME}.lnk"
  Call un.DeleteOwnedShortcut
  DeleteRegKey HKCU "${DILLA_UNINSTALL_KEY}"
  Delete "$INSTDIR\Uninstall ${PRODUCT_NAME}.exe"
  RMDir "$INSTDIR"
  Call un.ReleaseOperation
  Return
  Fail:
    Call un.Fail
SectionEnd

; Maintenance snapshots trust the current user, Administrators and SYSTEM.
; This mutex excludes cooperating NSIS/Go entry paths, not hostile same-user code.
!macro OperationFunctions Prefix
Function ${Prefix}AcquireOperation
  System::Call 'kernel32::CreateMutexW(p 0, i 0, w "Local\Dilla.Install.${APP_ID}")p.r0'
  StrCmp $0 0 0 +2
    Call ${Prefix}Fail
  StrCpy $OperationMutex $0
  System::Call 'kernel32::WaitForSingleObject(p r0, i 5000)i.r1'
  ${If} $1 != 0
  ${AndIf} $1 != 128
    Call ${Prefix}Fail
  ${EndIf}
FunctionEnd
Function ${Prefix}ReleaseOperation
  Call ${Prefix}ReleaseStableLauncher
  ${If} $OperationMutex != 0
    System::Call 'kernel32::ReleaseMutex(p $OperationMutex)'
    System::Call 'kernel32::CloseHandle(p $OperationMutex)'
    StrCpy $OperationMutex 0
  ${EndIf}
FunctionEnd
; Holding this fixed owned file denies new read/execute mappings while still
; allowing the installer itself to rename/delete it. Existing mapped images make
; GENERIC_WRITE fail before any archive/deletion. This is not a path-taking ABI.
Function ${Prefix}HoldStableLauncher
  System::Call 'kernel32::CreateFileW(w "$INSTDIR\Dilla.exe", i 0x40010000, i 4, p 0, i 3, i 0x80, p 0)p.r0'
  StrCmp $0 -1 0 +2
    Call ${Prefix}Fail
  StrCpy $StableLauncherHandle $0
FunctionEnd
Function ${Prefix}ReleaseStableLauncher
  ${If} $StableLauncherHandle != 0
    System::Call 'kernel32::CloseHandle(p $StableLauncherHandle)'
    StrCpy $StableLauncherHandle 0
  ${EndIf}
FunctionEnd
Function ${Prefix}GenerateGUID
  Push $0
  Push $1
  Push $2
  Push $3
  System::Alloc 16
  Pop $0
  StrCmp $0 0 0 +2
    Call ${Prefix}Fail
  System::Call 'ole32::CoCreateGuid(p r0)i.r1'
  ${If} $1 != 0
    System::Free $0
    Call ${Prefix}Fail
  ${EndIf}
  System::Call 'ole32::StringFromGUID2(p r0, w .r2, i 40)i.r3'
  System::Free $0
  StrCmp $3 39 +2
    Call ${Prefix}Fail
  StrCpy $GeneratedGUID $2
  Pop $3
  Pop $2
  Pop $1
  Pop $0
FunctionEnd
!macroend
!insertmacro OperationFunctions ""
!insertmacro OperationFunctions "un."

Function StagePreflight
  SetOutPath "$PLUGINSDIR\maintenance"
  ClearErrors
  CopyFiles /SILENT "$INSTDIR\Dilla.exe" "$PLUGINSDIR\maintenance\Dilla.exe"
  IfErrors Fail
  ExecWait '$\"$PLUGINSDIR\maintenance\Dilla.exe$\" --installer-preflight=stage --package-manifest-sha512=${DILLA_MANIFEST_SHA512}' $0
  IfErrors Fail
  StrCmp $0 0 +2
    Goto Fail
  Return
  Fail:
    Call Fail
FunctionEnd

Function PrepareInitialInstallation
  ClearErrors
  ExecWait '$\"$PLUGINSDIR\payload\Dilla.exe$\" --installer-preflight=initial' $0
  IfErrors Fail
  StrCmp $0 0 done
  StrCmp $0 10 +2
    Goto Fail
  StrCpy $InitialRetry 1
  ; Native preflight authenticated the same-package subset and marker. Prove
  ; every existing program image is writable BEFORE either namespace moves.
  IfFileExists "$INSTDIR\Dilla.exe" 0 +3
    Push "$INSTDIR\Dilla.exe"
    Call RequireUnlockedFile
  System::Call 'kernel32::GetFileAttributesW(w "$INSTDIR")i.r0'
  ${If} $0 != -1
    Push "$INSTDIR"
    Call CheckTree
  ${EndIf}
  System::Call 'kernel32::GetFileAttributesW(w "$DataRoot")i.r0'
  ${If} $0 != -1
    Push "$DataRoot"
    Call CheckTree
  ${EndIf}
  ; Recheck for an active update published by a parent that exited during the
  ; scan. No cooperating new app can start while the operation mutex is held.
  ClearErrors
  ExecWait '$\"$PLUGINSDIR\payload\Dilla.exe$\" --installer-preflight=initial' $0
  IfErrors Fail
  StrCmp $0 10 +2
    Goto Fail
  IfFileExists "$INSTDIR\Dilla.exe" 0 +2
    Call HoldStableLauncher
  Call GenerateGUID
  StrCpy $ProgramArchive "$LOCALAPPDATA\Programs\${PRODUCT_NAME}-incomplete-$GeneratedGUID"
  Push "$ProgramArchive"
  Call RequireAbsentPath
  System::Call 'kernel32::GetFileAttributesW(w "$INSTDIR")i.r0'
  ${If} $0 != -1
    ClearErrors
    Rename "$INSTDIR" "$ProgramArchive"
    IfErrors Fail
  ${EndIf}
  StrCpy $DataArchive "$APPDATA\${PRODUCT_NAME}\updates-incomplete-$GeneratedGUID"
  Push "$DataArchive"
  Call RequireAbsentPath
  System::Call 'kernel32::GetFileAttributesW(w "$DataRoot")i.r0'
  ${If} $0 != -1
    ClearErrors
    Rename "$DataRoot" "$DataArchive"
    IfErrors Fail
  ${EndIf}
  done:
    Call ReleaseStableLauncher
    Return
  Fail:
    Call Fail
FunctionEnd
Function CommitInitialMarker
  ${If} $InitialRetry == 1
    Return
  ${EndIf}
  ; Commit this fixed private ownership record before creating program roots.
  CreateDirectory "$APPDATA\${PRODUCT_NAME}"
  Call GenerateGUID
  StrCpy $2 "$APPDATA\${PRODUCT_NAME}\initial-install-$GeneratedGUID.tmp"
  Push "$2"
  Call RequireAbsentPath
  Push "$APPDATA\${PRODUCT_NAME}\initial-install.json"
  Call RequireAbsentPath
  ClearErrors
  FileOpen $0 "$2" w
  IfErrors Fail
  FileWrite $0 '{"format":1,"kind":"dilla-initial-install","appId":"${APP_ID}","manifestSha512":"${DILLA_MANIFEST_SHA512}"}'
  IfErrors Fail
  System::Call 'kernel32::FlushFileBuffers(p r0)i.r1'
  FileClose $0
  StrCmp $1 0 Fail
  ; Same-volume atomic publication requests write-through; hardware/power-loss
  ; directory-entry durability still requires the native release acceptance gate.
  System::Call 'kernel32::MoveFileExW(w "$2", w "$APPDATA\${PRODUCT_NAME}\initial-install.json", i 8)i.r0'
  StrCmp $0 0 Fail
  Return
  Fail:
    Call Fail
FunctionEnd
Function RestoreInitialArchives
  ${If} $InitialRetry == 1
    System::Call 'kernel32::GetFileAttributesW(w "$INSTDIR")i.r0'
    ${If} $0 == -1
    ${AndIf} $ProgramArchive != ""
      Rename "$ProgramArchive" "$INSTDIR"
    ${EndIf}
    System::Call 'kernel32::GetFileAttributesW(w "$DataRoot")i.r0'
    ${If} $0 == -1
    ${AndIf} $DataArchive != ""
      Rename "$DataArchive" "$DataRoot"
    ${EndIf}
  ${EndIf}
FunctionEnd

Function un.MaintenancePreflight
  SetOutPath "$PLUGINSDIR\maintenance"
  ClearErrors
  ; Embed the original compiled guard in the uninstaller. Damaged/missing root
  ; Go bytes never supply executable code or filesystem authority for removal.
  File /oname=Dilla.exe "${DILLA_BOOTSTRAP_EXE}"
  IfErrors Fail
  ExecWait '$\"$PLUGINSDIR\maintenance\Dilla.exe$\" --installer-preflight=uninstall' $0
  IfErrors Fail
  StrCmp $0 0 +2
    Goto Fail
  Return
  Fail:
    Call un.Fail
FunctionEnd
Function un.ArchiveHistory
  Call un.GenerateGUID
  StrCpy $0 "$APPDATA\${PRODUCT_NAME}\updates-uninstalled-$GeneratedGUID"
  Push "$0"
  Call un.CheckPath
  System::Call 'kernel32::GetFileAttributesW(w r0)i.r1'
  StrCmp $1 -1 +2
    Goto Fail
  System::Call 'kernel32::GetFileAttributesW(w "$DataRoot")i.r1'
  ClearErrors
  ${If} $1 == -1
    CreateDirectory "$0"
  ${Else}
    StrCpy $ArchivedUpdatesExisting 1
    Rename "$DataRoot" "$0"
  ${EndIf}
  IfErrors Fail
  StrCpy $DataArchive $0
  IfFileExists "$APPDATA\${PRODUCT_NAME}\initial-install.json" 0 done
  Rename "$APPDATA\${PRODUCT_NAME}\initial-install.json" "$DataArchive\initial-install.json"
  IfErrors Fail
  StrCpy $InitialMarkerArchived 1
  done:
    Return
  Fail:
    Call un.Fail
FunctionEnd
Function un.RestoreHistory
  ${If} $DataArchive != ""
    ${If} $InitialMarkerArchived == 1
      IfFileExists "$APPDATA\${PRODUCT_NAME}\initial-install.json" +2
        Rename "$DataArchive\initial-install.json" "$APPDATA\${PRODUCT_NAME}\initial-install.json"
    ${EndIf}
    System::Call 'kernel32::GetFileAttributesW(w "$DataRoot")i.r0'
    ${If} $0 == -1
    ${AndIf} $ArchivedUpdatesExisting == 1
      Rename "$DataArchive" "$DataRoot"
    ${EndIf}
  ${EndIf}
FunctionEnd
