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
Var PayloadPins
Var PayloadPinCount
Var PayloadHandle
Var PayloadInfo
Var PayloadToken
Var PayloadTokenInfo
Var PayloadSIDString
Var PayloadUserSID
Var PayloadDriveRoot
Var PayloadDescriptor
Var PayloadNameBuffer
Var PayloadObjectName
Var PayloadObjectAttributes
Var PayloadIOStatus
Var PayloadCreatedHandle
Var PayloadOwnerDescriptor
Var PayloadOwnerSIDString
Var PayloadDACLString
Var PayloadFindHandle
Var PayloadFindData
Var PayloadCleanupFailed

Function Fail
  Call ReleasePrivatePayload
  Call ReleaseStableLauncher
  Call RestoreInitialArchives
  Call ReleaseOperation
  MessageBox MB_OK|MB_ICONSTOP "Dilla setup could not safely complete. Quit Dilla from its tray menu and retry. Existing device data is preserved." /SD IDOK
  SetErrorLevel 2
  Quit
FunctionEnd
Function un.Fail
  Call un.ReleasePrivatePayload
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
  Call InitializePrivatePayload
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
  Call PreparePrivatePayload
  SetOutPath "$PLUGINSDIR\payload"
  File /oname=$PLUGINSDIR\payload\payload.7z "${APP_64}"
  ClearErrors
  Nsis7z::Extract "$PLUGINSDIR\payload\payload.7z"
  IfErrors 0 +2
    Call Fail
  ClearErrors
  Delete "$PLUGINSDIR\payload\payload.7z"
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
  Call un.InitializePrivatePayload
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
  Call ${Prefix}ReleasePrivatePayload
  IfErrors 0 +2
    Call ${Prefix}Fail
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
  SetOutPath "$PLUGINSDIR\payload\maintenance"
  ClearErrors
  CopyFiles /SILENT "$INSTDIR\Dilla.exe" "$PLUGINSDIR\payload\maintenance\Dilla.exe"
  IfErrors Fail
  ExecWait '$\"$PLUGINSDIR\payload\maintenance\Dilla.exe$\" --installer-preflight=stage --package-manifest-sha512=${DILLA_MANIFEST_SHA512}' $0
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
  ${If} $PayloadPins == 0
    Call un.PreparePrivatePayload
  ${EndIf}
  SetOutPath "$PLUGINSDIR\payload\maintenance"
  ClearErrors
  ; Embed the original compiled guard in the uninstaller. Damaged/missing root
  ; Go bytes never supply executable code or filesystem authority for removal.
  File /oname=Dilla.exe "${DILLA_BOOTSTRAP_EXE}"
  IfErrors Fail
  ExecWait '$\"$PLUGINSDIR\payload\maintenance\Dilla.exe$\" --installer-preflight=uninstall' $0
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

!macro FreePayloadLocal Variable
    ${If} ${Variable} != 0
      System::Call 'kernel32::LocalFree(p ${Variable})p.r0'
      ${If} $0 != 0
        StrCpy $PayloadCleanupFailed 1
      ${EndIf}
      StrCpy ${Variable} 0
    ${EndIf}
!macroend
!macro FreePayloadBuffer Variable
    ${If} ${Variable} != 0
      System::Free ${Variable}
      StrCpy ${Variable} 0
    ${EndIf}
!macroend

!define /math DILLA_OBJECT_ATTRIBUTES_SIZE ${NSIS_PTR_SIZE} * 6
!macro PrivatePayloadFunctions Prefix
Function ${Prefix}InitializePrivatePayload
  StrCpy $PayloadPins 0
  StrCpy $PayloadPinCount 0
  StrCpy $PayloadHandle 0
  StrCpy $PayloadInfo 0
  StrCpy $PayloadToken 0
  StrCpy $PayloadTokenInfo 0
  StrCpy $PayloadSIDString 0
  StrCpy $PayloadDescriptor 0
  StrCpy $PayloadNameBuffer 0
  StrCpy $PayloadObjectName 0
  StrCpy $PayloadObjectAttributes 0
  StrCpy $PayloadIOStatus 0
  StrCpy $PayloadCreatedHandle 0
  StrCpy $PayloadOwnerDescriptor 0
  StrCpy $PayloadOwnerSIDString 0
  StrCpy $PayloadDACLString 0
  StrCpy $PayloadFindHandle -1
  StrCpy $PayloadFindData 0
  StrCpy $PayloadCleanupFailed 0
FunctionEnd

; NSIS's plug-in directory can inherit a WORLD DELETE_CHILD grant. The payload
; must not inherit it. Protect a NEW child before writing any executable/archive
; and retain no-delete-sharing pins for its entire existing ancestor chain.
Function ${Prefix}PreparePrivatePayload
  StrCpy $PayloadCleanupFailed 0
  StrCmp $PayloadPins 0 +2
    Call ${Prefix}Fail
  ; Only a local drive-absolute namespace is supported. Never pin C: (the
  ; drive's current directory), a UNC/device path or a mapped remote drive.
  StrCpy $0 "$PLUGINSDIR" 2
  StrCmp $0 "\\" Fail
  StrCpy $0 "$PLUGINSDIR" 1 1
  StrCmp $0 ":" 0 Fail
  StrCpy $0 "$PLUGINSDIR" 1 2
  StrCmp $0 "\" 0 Fail
  StrCpy $PayloadDriveRoot "$PLUGINSDIR" 3
  System::Call 'kernel32::GetDriveTypeW(w "$PayloadDriveRoot")i.r0'
  ${If} $0 != 2
  ${AndIf} $0 != 3
  ${AndIf} $0 != 5
  ${AndIf} $0 != 6
    Goto Fail
  ${EndIf}
  System::Call 'kernel32::GetCurrentProcess()p.r0'
  System::Call 'advapi32::OpenProcessToken(p r0, i 8, *p.r1)i.r2'
  StrCmp $2 0 Fail
  StrCpy $PayloadToken $1
  System::Call 'advapi32::GetTokenInformation(p $PayloadToken, i 1, p 0, i 0, *i.r1)i.r0 ?e'
  Pop $2
  StrCmp $0 0 0 Fail
  StrCmp $2 122 0 Fail
  IntCmp $1 8 Fail Fail +1
  IntCmp $1 65536 +1 +1 Fail
  System::Alloc $1
  Pop $PayloadTokenInfo
  StrCmp $PayloadTokenInfo 0 Fail
  System::Call 'advapi32::GetTokenInformation(p $PayloadToken, i 1, p $PayloadTokenInfo, i r1, *i.r2)i.r0'
  StrCmp $0 0 Fail
  IntCmp $2 $1 +1 +1 Fail
  IntCmp $2 8 +1 Fail +1
  System::Call '*$PayloadTokenInfo(p.r0)'
  System::Call 'advapi32::IsValidSid(p r0)i.r2'
  StrCmp $2 0 Fail
  System::Call 'advapi32::ConvertSidToStringSidW(p r0, *p.r1)i.r2'
  StrCmp $2 0 Fail
  StrCpy $PayloadSIDString $1
  System::Call 'kernel32::lstrcpynW(w .r9, p $PayloadSIDString, i ${NSIS_MAX_STRLEN})p.r0'
  StrCmp $0 0 Fail
  StrCpy $PayloadUserSID $9
  System::Alloc 52
  Pop $PayloadInfo
  StrCmp $PayloadInfo 0 Fail
  ; Build the existing path list, then open ROOT -> leaf. Each checked directory
  ; remains pinned against rename for the entire lifetime of the payload.
  StrCpy $R0 "$PLUGINSDIR"
  StrCpy $R2 0
  StrCpy $R3 "$PayloadDriveRoot" 2
  collect:
    Push $R0
    IntOp $R2 $R2 + 1
    IntCmp $R2 256 +1 +1 Fail
    StrCmp $R0 $PayloadDriveRoot pin
    ${GetParent} "$R0" $R1
    ; FileFunc GetParent(C:\Users) returns C:, not the drive root C:\.
    StrCmp $R1 $R3 normalize_root
    StrCmp $R1 "" Fail
    StrCmp $R1 $R0 Fail
    StrCpy $R0 $R1
    Goto collect
  normalize_root:
    StrCpy $R0 $PayloadDriveRoot
    Goto collect
  pin:
    Pop $R0
    Push $R0
    Call ${Prefix}PinPayloadDirectory
    IntOp $R2 $R2 - 1
    StrCmp $R2 0 +2
      Goto pin
  ; Explicit owner and a protected, inheritable DACL: only the current token
  ; user, SYSTEM and Administrators can mutate this newly created namespace.
  StrCpy $9 "O:$PayloadUserSID"
  StrCpy $9 "$9D:P(A;OICI;FA;;;$PayloadUserSID)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
  System::Call 'advapi32::ConvertStringSecurityDescriptorToSecurityDescriptorW(w r9, i 1, *p.r0, p 0)i.r1'
  StrCmp $1 0 Fail
  StrCpy $PayloadDescriptor $0
  ; Constant relative UNICODE_STRING: Length=14, MaximumLength=16. Pointer
  ; slots include the ABI's required padding on x64 (8/16 bytes total).
  System::Alloc 16
  Pop $PayloadNameBuffer
  StrCmp $PayloadNameBuffer 0 Fail
  System::Call 'kernel32::lstrcpynW(p $PayloadNameBuffer, w "payload", i 8)p.r0'
  StrCmp $0 0 Fail
  System::Call '*(p 0x0010000e, p $PayloadNameBuffer)p.r0'
  StrCmp $0 0 Fail
  StrCpy $PayloadObjectName $0
  ; OBJECT_ATTRIBUTES is six aligned slots (24/48 bytes). RootDirectory is
  ; the already checked/pinned PLUGINSDIR; no mutable absolute lookup occurs.
  System::Call '*$PayloadPins(p.r0, p.r1)'
  System::Call '*(p ${DILLA_OBJECT_ATTRIBUTES_SIZE}, p r1, p $PayloadObjectName, p 0x40, p $PayloadDescriptor, p 0)p.r0'
  StrCmp $0 0 Fail
  StrCpy $PayloadObjectAttributes $0
  System::Call '*(p 0, p 0)p.r0'
  StrCmp $0 0 Fail
  StrCpy $PayloadIOStatus $0
  ; FILE_TRAVERSE engages Windows share accounting; metadata-only handles do not.
  ; FILE_CREATE=2 never adopts any existing object. Creation and its first
  ; no-delete-sharing handle are one operation, eliminating a replace window.
  StrCpy $0 0
  System::Call 'ntdll::NtCreateFile(*p.r0, i 0x1200A0, p $PayloadObjectAttributes, p $PayloadIOStatus, p 0, i 128, i 3, i 2, i 0x200021, p 0, i 0)i.r1'
  StrCpy $PayloadCreatedHandle $0
  StrCmp $1 0 0 Fail
  StrCmp $PayloadCreatedHandle 0 Fail
  StrCmp $PayloadCreatedHandle -1 Fail
  System::Call '*$PayloadIOStatus(p.r0, p.r1)'
  StrCmp $0 0 0 Fail
  StrCmp $1 2 0 Fail ; FILE_CREATED (FILE_OPENED=1 must never be accepted).
  Push "$PLUGINSDIR\payload"
  Call ${Prefix}PinPayloadDirectory
  ; The atomic handle stays open until the second checked pin is registered.
  ; Verify the actual filesystem enforced our requested descriptor as well.
  Call ${Prefix}VerifyPrivatePayload
  ; Filesystems that ignore the supplied DACL cannot establish this contract.
  System::Call '*$PayloadPins(p.r0, p.r1)'
  System::Call 'kernel32::GetVolumeInformationByHandleW(p r1, p 0, i 0, p 0, p 0, *i.r2, p 0, i 0)i.r0'
  StrCmp $0 0 Fail
  IntOp $2 $2 & 8
  StrCmp $2 8 0 Fail
  Call ${Prefix}FreePayloadSecurity
  StrCmp $PayloadCleanupFailed 0 +2
    Goto Fail
  Return
  Fail:
    Call ${Prefix}Fail
FunctionEnd

Function ${Prefix}VerifyPrivatePayload
  System::Call '*$PayloadPins(p.r0, p.r1)'
  System::Call 'advapi32::GetSecurityInfo(p r1, i 1, i 5, *p.r0, p 0, p 0, p 0, *p.r1)i.r2'
  StrCmp $2 0 0 Fail
  StrCpy $PayloadOwnerDescriptor $1
  StrCmp $0 0 Fail
  System::Call '*$PayloadTokenInfo(p.r2)'
  System::Call 'advapi32::EqualSid(p r0, p r2)i.r3'
  StrCmp $3 0 Fail
  System::Call 'advapi32::GetSecurityDescriptorControl(p $PayloadOwnerDescriptor, *i.r0, *i.r1)i.r2'
  StrCmp $2 0 Fail
  IntOp $0 $0 & 0x1000
  StrCmp $0 4096 0 Fail
  ; Compare the entire canonical ACE body against our in-process descriptor.
  ; Only the known protected/auto-inherited control spelling may differ.
  StrCpy $R0 $PayloadDescriptor
  StrCpy $R1 0
  descriptor:
    System::Call 'advapi32::ConvertSecurityDescriptorToStringSecurityDescriptorW(p $R0, i 1, i 4, *p.r0, *i.r1)i.r2'
    StrCmp $2 0 Fail
    StrCpy $PayloadDACLString $0
    IntCmp $1 1 Fail Fail +1
    IntCmp $1 ${NSIS_MAX_STRLEN} +1 +1 Fail
    System::Call 'kernel32::lstrcpynW(w .r3, p $PayloadDACLString, i ${NSIS_MAX_STRLEN})p.r0'
    StrCmp $0 0 Fail
    System::Call 'kernel32::LocalFree(p $PayloadDACLString)p.r0'
    StrCmp $0 0 0 Fail
    StrCpy $PayloadDACLString 0
    StrCpy $0 $3 4
    StrCmp $0 "D:P(" protected
    StrCpy $0 $3 6
    StrCmp $0 "D:PAI(" 0 Fail
    StrCpy $3 $3 "" 5
    Goto canonical
    protected:
      StrCpy $3 $3 "" 3
    canonical:
    StrCmp $R1 0 expected
    StrCmp $3 $R2 0 Fail
    Goto verified
    expected:
      StrCpy $R2 $3
      StrCpy $R0 $PayloadOwnerDescriptor
      StrCpy $R1 1
      Goto descriptor
  verified:
  Call ${Prefix}RequireEmptyPayload
  System::Call 'kernel32::LocalFree(p $PayloadOwnerDescriptor)p.r0'
  StrCmp $0 0 0 Fail
  StrCpy $PayloadOwnerDescriptor 0
  Return
  Fail:
    Call ${Prefix}Fail
FunctionEnd

Function ${Prefix}RequireEmptyPayload
  ; WIN32_FIND_DATAW is 592 bytes; cFileName starts at byte 44.
  System::Alloc 592
  Pop $PayloadFindData
  StrCmp $PayloadFindData 0 Fail
  System::Call 'kernel32::FindFirstFileW(w "$PLUGINSDIR\payload\*", p $PayloadFindData)p.r0 ?e'
  Pop $1
  StrCpy $PayloadFindHandle $0
  StrCmp $0 -1 0 entry
  StrCmp $1 2 done Fail
  entry:
    System::Int64Op $PayloadFindData + 44
    Pop $0
    System::Call 'kernel32::lstrcpynW(w .r1, p r0, i 260)p.r2'
    StrCmp $2 0 Fail
    StrCmp $1 "." next
    StrCmp $1 ".." 0 Fail
    next:
      System::Call 'kernel32::FindNextFileW(p $PayloadFindHandle, p $PayloadFindData)i.r0 ?e'
      Pop $1
      StrCmp $0 0 0 entry
      StrCmp $1 18 0 Fail
  done:
  ${If} $PayloadFindHandle != -1
    System::Call 'kernel32::FindClose(p $PayloadFindHandle)i.r0'
    StrCmp $0 0 Fail
    StrCpy $PayloadFindHandle -1
  ${EndIf}
  System::Free $PayloadFindData
  StrCpy $PayloadFindData 0
  Return
  Fail:
    Call ${Prefix}Fail
FunctionEnd

Function ${Prefix}PinPayloadDirectory
  Exch $0
  Push $1
  Push $2
  Push $3
  ; READ_CONTROL|READ_ATTRIBUTES|FILE_TRAVERSE, READ|WRITE sharing, NO DELETE sharing.
  ; Traverse participates in share accounting without requiring directory listing.
  System::Call 'kernel32::CreateFileW(w r0, i 0x200A0, i 3, p 0, i 3, i 0x02200000, p 0)p.r1'
  StrCmp $1 -1 Fail
  StrCpy $PayloadHandle $1
  System::Call 'kernel32::GetFileInformationByHandle(p $PayloadHandle, p $PayloadInfo)i.r2'
  StrCmp $2 0 Fail
  System::Call '*$PayloadInfo(i.r2)'
  IntOp $3 $2 & 0x400
  StrCmp $3 0 0 Fail
  IntOp $3 $2 & 0x10
  StrCmp $3 16 0 Fail
  ; A substituted other-user plug-in directory is not our newly created root.
  StrCmp $0 "$PLUGINSDIR" 0 owned
    Call ${Prefix}VerifyPayloadPluginOwner
  owned:
  System::Call '*(p $PayloadPins, p $PayloadHandle)p.r2'
  StrCmp $2 0 Fail
  StrCpy $PayloadPins $2
  StrCpy $PayloadHandle 0
  IntOp $PayloadPinCount $PayloadPinCount + 1
  IntCmp $PayloadPinCount 257 Fail +1 Fail
  Pop $3
  Pop $2
  Pop $1
  Pop $0
  Return
  Fail:
    Call ${Prefix}Fail
FunctionEnd

Function ${Prefix}VerifyPayloadPluginOwner
  Push $0
  Push $1
  Push $2
  Push $3
  System::Call 'advapi32::GetSecurityInfo(p $PayloadHandle, i 1, i 1, *p.r0, p 0, p 0, p 0, *p.r1)i.r2'
  StrCmp $2 0 0 Fail
  StrCpy $PayloadOwnerDescriptor $1
  StrCmp $0 0 Fail
  System::Call 'advapi32::ConvertSidToStringSidW(p r0, *p.r1)i.r2'
  StrCmp $2 0 Fail
  StrCpy $PayloadOwnerSIDString $1
  ; Keep the original TokenUser SID string alive until final resource cleanup.
  System::Call 'kernel32::lstrcpynW(w .r3, p $PayloadOwnerSIDString, i ${NSIS_MAX_STRLEN})p.r2'
  StrCmp $2 0 Fail
  System::Call 'kernel32::LocalFree(p $PayloadOwnerSIDString)p.r2'
  StrCmp $2 0 0 Fail
  StrCpy $PayloadOwnerSIDString 0
  StrCmp $3 $PayloadUserSID trusted
  StrCmp $3 "S-1-5-18" trusted
  StrCmp $3 "S-1-5-32-544" trusted
  Goto Fail
  trusted:
  System::Call 'kernel32::LocalFree(p $PayloadOwnerDescriptor)p.r0'
  StrCmp $0 0 0 Fail
  StrCpy $PayloadOwnerDescriptor 0
  Pop $3
  Pop $2
  Pop $1
  Pop $0
  Return
  Fail:
    Call ${Prefix}Fail
FunctionEnd

Function ${Prefix}FreePayloadSecurity
  ${If} $PayloadCreatedHandle != 0
    System::Call 'kernel32::CloseHandle(p $PayloadCreatedHandle)i.r0'
    ${If} $0 == 0
      StrCpy $PayloadCleanupFailed 1
    ${EndIf}
    StrCpy $PayloadCreatedHandle 0
  ${EndIf}
  ${If} $PayloadFindHandle != -1
    System::Call 'kernel32::FindClose(p $PayloadFindHandle)i.r0'
    ${If} $0 == 0
      StrCpy $PayloadCleanupFailed 1
    ${EndIf}
    StrCpy $PayloadFindHandle -1
  ${EndIf}
  ${If} $PayloadHandle != 0
    System::Call 'kernel32::CloseHandle(p $PayloadHandle)i.r0'
    ${If} $0 == 0
      StrCpy $PayloadCleanupFailed 1
    ${EndIf}
    StrCpy $PayloadHandle 0
  ${EndIf}
  ${If} $PayloadToken != 0
    System::Call 'kernel32::CloseHandle(p $PayloadToken)i.r0'
    ${If} $0 == 0
      StrCpy $PayloadCleanupFailed 1
    ${EndIf}
    StrCpy $PayloadToken 0
  ${EndIf}
  !insertmacro FreePayloadLocal $PayloadSIDString
  !insertmacro FreePayloadLocal $PayloadDescriptor
  !insertmacro FreePayloadLocal $PayloadOwnerDescriptor
  !insertmacro FreePayloadLocal $PayloadOwnerSIDString
  !insertmacro FreePayloadLocal $PayloadDACLString
  !insertmacro FreePayloadBuffer $PayloadTokenInfo
  !insertmacro FreePayloadBuffer $PayloadNameBuffer
  !insertmacro FreePayloadBuffer $PayloadObjectName
  !insertmacro FreePayloadBuffer $PayloadObjectAttributes
  !insertmacro FreePayloadBuffer $PayloadIOStatus
  !insertmacro FreePayloadBuffer $PayloadInfo
  !insertmacro FreePayloadBuffer $PayloadFindData
FunctionEnd

Function ${Prefix}ReleasePrivatePayload
  StrCpy $PayloadCleanupFailed 0
  Call ${Prefix}FreePayloadSecurity
  loop:
    StrCmp $PayloadPins 0 done
    System::Call '*$PayloadPins(p.r0, p.r1)'
    System::Free $PayloadPins
    StrCpy $PayloadPins $0
    System::Call 'kernel32::CloseHandle(p r1)i.r0'
    ${If} $0 == 0
      StrCpy $PayloadCleanupFailed 1
    ${EndIf}
    Goto loop
  done:
    StrCpy $PayloadPinCount 0
    ClearErrors
    StrCmp $PayloadCleanupFailed 0 +2
      SetErrors
FunctionEnd

!macroend
!insertmacro PrivatePayloadFunctions ""
!insertmacro PrivatePayloadFunctions "un."

Function .onGUIEnd
  Call ReleasePrivatePayload
  IfErrors 0 +2
    SetErrorLevel 2
FunctionEnd

Function un.onGUIEnd
  Call un.ReleasePrivatePayload
  IfErrors 0 +2
    SetErrorLevel 2
FunctionEnd
