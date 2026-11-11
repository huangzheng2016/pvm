; pvm-setup.nsi: per-user Windows installer for pvm and its bundled QEMU
; runtime. Built by script/windows/pack-setup.sh; do not run makensis directly.
;
; Required defines: VERSION, PVM_EXE, RUNTIME_DIR, OUTFILE.

Unicode true
SetCompressor /SOLID lzma
RequestExecutionLevel user
ManifestDPIAware true

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "WinMessages.nsh"

!define UNINST_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\pvm"

Name "pvm ${VERSION}"
OutFile "${OUTFILE}"
InstallDir "$LOCALAPPDATA\pvm"
ShowInstDetails show
ShowUninstDetails show

!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_TEXT "pvm ${VERSION} is installed.$\r$\n$\r$\nOpen a new terminal and run:$\r$\n  pvm create --image ubuntu:26.04 --name dev1 --user dev"
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Var RuntimeRoot

; Stops a running daemon through the given pvm.exe. Running VMs stop with it,
; so ask first; silent installs refuse instead of stopping VMs unasked.
!macro StopDaemon un
Function ${un}StopDaemon
  Exch $0
  ${IfNot} ${FileExists} "$0"
    Pop $0
    Return
  ${EndIf}
  nsExec::ExecToStack /OEM '"$0" daemon status'
  Pop $1
  Pop $2
  StrCpy $2 $2 15
  ${If} $2 == "daemon: running"
    MessageBox MB_OKCANCEL|MB_ICONEXCLAMATION "The pvm daemon is running. It must be stopped to continue, which also stops all running VMs.$\r$\n$\r$\nStop it now?" /SD IDCANCEL IDOK stop
    Abort "pvm daemon is running; stop it with 'pvm daemon stop' and retry"
    stop:
    DetailPrint "Stopping pvm daemon..."
    nsExec::ExecToLog '"$0" daemon stop'
    Pop $1
    ${If} $1 != 0
      Abort "'pvm daemon stop' failed (exit $1)"
    ${EndIf}
  ${EndIf}
  Pop $0
FunctionEnd
!macroend
!insertmacro StopDaemon ""
!insertmacro StopDaemon "un."

!macro UserPath action dir
  InitPluginsDir
  File "/oname=$PLUGINSDIR\user-path.ps1" "${__FILEDIR__}\user-path.ps1"
  nsExec::ExecToLog 'powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$PLUGINSDIR\user-path.ps1" -Action ${action} -Dir "${dir}"'
  Pop $1
  ${If} $1 != 0
    Abort "updating the user PATH failed (exit $1)"
  ${EndIf}
  SendMessage ${HWND_BROADCAST} ${WM_SETTINGCHANGE} 0 "STR:Environment" /TIMEOUT=5000
!macroend

Section "pvm"
  ReadEnvStr $RuntimeRoot PVM_HOME
  ${If} $RuntimeRoot == ""
    StrCpy $RuntimeRoot "$PROFILE\.pvm"
  ${EndIf}
  StrCpy $RuntimeRoot "$RuntimeRoot\runtime\qemu"

  Push "$INSTDIR\pvm.exe"
  Call StopDaemon

  ; Stage both components and verify them before replacing anything.
  DetailPrint "Staging pvm ${VERSION}..."
  SetOutPath "$INSTDIR"
  File "/oname=pvm.new.exe" "${PVM_EXE}"
  nsExec::ExecToStack /OEM '"$INSTDIR\pvm.new.exe" version'
  Pop $1
  Pop $2
  StrLen $3 "${VERSION}"
  StrCpy $2 $2 $3
  ${If} $1 != 0
  ${OrIf} $2 != "${VERSION}"
    Delete "$INSTDIR\pvm.new.exe"
    Abort "staged pvm.exe does not report version ${VERSION}"
  ${EndIf}

  DetailPrint "Staging QEMU runtime..."
  RMDir /r "$RuntimeRoot\.x86_64.new"
  SetOutPath "$RuntimeRoot\.x86_64.new"
  File /r "${RUNTIME_DIR}\*.*"
  ; Leave the staged directory: Windows cannot rename a process's cwd.
  SetOutPath "$INSTDIR"
  nsExec::ExecToStack /OEM '"$RuntimeRoot\.x86_64.new\bin\qemu-system-x86_64.exe" --version'
  Pop $1
  Pop $2
  ${If} $1 != 0
    RMDir /r "$RuntimeRoot\.x86_64.new"
    Delete "$INSTDIR\pvm.new.exe"
    Abort "staged qemu-system-x86_64.exe failed to run (exit $1)"
  ${EndIf}

  ; Swap the runtime, keeping the previous one until the binary is in place.
  RMDir /r "$RuntimeRoot\.x86_64.old"
  ${If} ${FileExists} "$RuntimeRoot\x86_64\*.*"
    ClearErrors
    Rename "$RuntimeRoot\x86_64" "$RuntimeRoot\.x86_64.old"
    ${If} ${Errors}
      RMDir /r "$RuntimeRoot\.x86_64.new"
      Delete "$INSTDIR\pvm.new.exe"
      Abort "cannot replace $RuntimeRoot\x86_64 (a QEMU process may still be using it)"
    ${EndIf}
  ${EndIf}
  Rename "$RuntimeRoot\.x86_64.new" "$RuntimeRoot\x86_64"

  Delete "$INSTDIR\pvm.old.exe"
  ${If} ${FileExists} "$INSTDIR\pvm.exe"
    ClearErrors
    Rename "$INSTDIR\pvm.exe" "$INSTDIR\pvm.old.exe"
    ${If} ${Errors}
      RMDir /r "$RuntimeRoot\x86_64"
      Rename "$RuntimeRoot\.x86_64.old" "$RuntimeRoot\x86_64"
      Delete "$INSTDIR\pvm.new.exe"
      Abort "cannot replace $INSTDIR\pvm.exe (is pvm still running?)"
    ${EndIf}
  ${EndIf}
  Rename "$INSTDIR\pvm.new.exe" "$INSTDIR\pvm.exe"
  Delete "$INSTDIR\pvm.old.exe"
  RMDir /r "$RuntimeRoot\.x86_64.old"
  DetailPrint "Installed $INSTDIR\pvm.exe and $RuntimeRoot\x86_64"

  DetailPrint "Adding $INSTDIR to the user PATH..."
  !insertmacro UserPath add "$INSTDIR"

  WriteUninstaller "$INSTDIR\uninstall.exe"
  WriteRegStr HKCU "${UNINST_KEY}" "DisplayName" "pvm"
  WriteRegStr HKCU "${UNINST_KEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${UNINST_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINST_KEY}" "RuntimeRoot" "$RuntimeRoot"
  WriteRegStr HKCU "${UNINST_KEY}" "DisplayIcon" "$INSTDIR\pvm.exe"
  WriteRegStr HKCU "${UNINST_KEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKCU "${UNINST_KEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegDWORD HKCU "${UNINST_KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINST_KEY}" "NoRepair" 1
SectionEnd

; Removes only what the installer placed: VM disks, keys and config under
; ~/.pvm are user data and stay.
Section "Uninstall"
  Push "$INSTDIR\pvm.exe"
  Call un.StopDaemon

  ReadRegStr $RuntimeRoot HKCU "${UNINST_KEY}" "RuntimeRoot"
  ${If} $RuntimeRoot != ""
    RMDir /r "$RuntimeRoot\x86_64"
    RMDir "$RuntimeRoot"
  ${EndIf}

  !insertmacro UserPath remove "$INSTDIR"

  Delete "$INSTDIR\pvm.exe"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  DeleteRegKey HKCU "${UNINST_KEY}"
  DetailPrint "VM data under ~/.pvm was kept; delete it manually if no longer needed."
SectionEnd
