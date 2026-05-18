; ses-smtp-proxy Windows installer
;
; Build (from repo root, on Linux or Windows):
;   makensis -DVERSION=1.0.0 -DARCH=amd64 -DEXE_PATH=dist/ses-smtp-proxy-amd64.exe \
;            -DOUT_FILE=dist/ses-smtp-proxy-1.0.0-amd64-setup.exe installer/installer.nsi
;
; Required defines:
;   VERSION   - human-readable version string written to the registry
;   ARCH      - target architecture (amd64 | arm64); recorded for diagnostics
;   EXE_PATH  - relative or absolute path to the pre-built ses-smtp-proxy.exe
;   OUT_FILE  - relative or absolute path of the installer .exe to produce

!ifndef VERSION
  !define VERSION "dev"
!endif
!ifndef ARCH
  !define ARCH "amd64"
!endif
!ifndef EXE_PATH
  !define EXE_PATH "..\dist\ses-smtp-proxy-${ARCH}.exe"
!endif
!ifndef OUT_FILE
  !define OUT_FILE "..\dist\ses-smtp-proxy-${VERSION}-${ARCH}-setup.exe"
!endif

!define APP_NAME            "ses-smtp-proxy"
!define DISPLAY_NAME        "SES to SMTP proxy"
!define PUBLISHER           "ses-smtp-proxy contributors"
!define WEBSITE             "https://github.com/TCANationals/ses-to-smtp-proxy"
!define SERVICE_NAME        "ses-smtp-proxy"
!define EXE_NAME            "ses-smtp-proxy.exe"
!define UNINSTALL_KEY       "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APP_NAME}"
!define DEFAULT_SMTP_PORT   "2525"

;------------------------------------------------------------------------------
; General configuration
;------------------------------------------------------------------------------

Name                "${DISPLAY_NAME} ${VERSION}"
OutFile             "${OUT_FILE}"
Unicode             true
RequestExecutionLevel admin
InstallDir          "$PROGRAMFILES64\${APP_NAME}"
InstallDirRegKey    HKLM "Software\${APP_NAME}" "InstallDir"
ShowInstDetails     show
ShowUninstDetails   show
SetCompressor       /SOLID lzma

VIProductVersion              "0.0.0.0"
VIAddVersionKey "ProductName"     "${DISPLAY_NAME}"
VIAddVersionKey "CompanyName"     "${PUBLISHER}"
VIAddVersionKey "FileDescription" "Installer for ${DISPLAY_NAME}"
VIAddVersionKey "FileVersion"     "${VERSION}"
VIAddVersionKey "ProductVersion"  "${VERSION}"
VIAddVersionKey "LegalCopyright"  "MIT-licensed open source software"

;------------------------------------------------------------------------------
; Modern UI 2
;------------------------------------------------------------------------------

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "FileFunc.nsh"

!define MUI_ABORTWARNING
!define MUI_ICON   "${NSISDIR}\Contrib\Graphics\Icons\modern-install.ico"
!define MUI_UNICON "${NSISDIR}\Contrib\Graphics\Icons\modern-uninstall.ico"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "..\LICENSE"
!insertmacro MUI_PAGE_COMPONENTS
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES

!define MUI_FINISHPAGE_RUN_NOTCHECKED
!define MUI_FINISHPAGE_SHOWREADME ""
!define MUI_FINISHPAGE_SHOWREADME_NOTCHECKED
!define MUI_FINISHPAGE_SHOWREADME_TEXT "Open config.yaml in Notepad"
!define MUI_FINISHPAGE_SHOWREADME_FUNCTION OpenConfigInNotepad
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

;------------------------------------------------------------------------------
; Helpers
;------------------------------------------------------------------------------

Var PreviousInstallDir
Var ConfigPath

Function .onInit
  StrCpy $ConfigPath "$INSTDIR\config.yaml"

  ; Detect a previous installation so we can stop+uninstall the old service
  ; before overwriting the exe.
  ReadRegStr $PreviousInstallDir HKLM "${UNINSTALL_KEY}" "InstallLocation"
FunctionEnd

; $INSTDIR is finalised after the directory page, so refresh $ConfigPath
; before we use it during install.
Function .onVerifyInstDir
  StrCpy $ConfigPath "$INSTDIR\config.yaml"
FunctionEnd

;------------------------------------------------------------------------------
; Components
;------------------------------------------------------------------------------

Section "Service binary (required)" SecService
  SectionIn RO

  ; Stop and uninstall an existing service before overwriting.
  ${If} $PreviousInstallDir != ""
    DetailPrint "Stopping existing service..."
    nsExec::ExecToLog 'sc.exe stop "${SERVICE_NAME}"'
    Pop $0
    Sleep 2000
    DetailPrint "Removing existing service registration..."
    nsExec::ExecToLog '"$PreviousInstallDir\${EXE_NAME}" uninstall'
    Pop $0
  ${EndIf}

  SetOutPath "$INSTDIR"
  File /oname=${EXE_NAME} "${EXE_PATH}"

  ; Make sure $ConfigPath reflects the user-chosen $INSTDIR.
  StrCpy $ConfigPath "$INSTDIR\config.yaml"

  ; Register the Windows service. This also installs an Event Log source so
  ; structured logging routes through the Windows Event Log when no
  ; logging.file is configured.
  DetailPrint "Registering Windows service..."
  nsExec::ExecToLog '"$INSTDIR\${EXE_NAME}" install'
  Pop $0
  ${If} $0 != 0
    DetailPrint "Service install returned exit code $0 (continuing)."
  ${EndIf}

  ; Add/Remove Programs entry.
  WriteRegStr HKLM "Software\${APP_NAME}" "InstallDir" "$INSTDIR"
  WriteRegStr HKLM "${UNINSTALL_KEY}" "DisplayName"      "${DISPLAY_NAME}"
  WriteRegStr HKLM "${UNINSTALL_KEY}" "DisplayVersion"   "${VERSION}"
  WriteRegStr HKLM "${UNINSTALL_KEY}" "Publisher"        "${PUBLISHER}"
  WriteRegStr HKLM "${UNINSTALL_KEY}" "InstallLocation"  "$INSTDIR"
  WriteRegStr HKLM "${UNINSTALL_KEY}" "DisplayIcon"      "$INSTDIR\${EXE_NAME}"
  WriteRegStr HKLM "${UNINSTALL_KEY}" "URLInfoAbout"     "${WEBSITE}"
  WriteRegStr HKLM "${UNINSTALL_KEY}" "UninstallString"  '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKLM "${UNINSTALL_KEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegDWORD HKLM "${UNINSTALL_KEY}" "NoModify" 1
  WriteRegDWORD HKLM "${UNINSTALL_KEY}" "NoRepair" 1

  ; Approximate install size in KB for the Add/Remove Programs listing.
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  WriteRegDWORD HKLM "${UNINSTALL_KEY}" "EstimatedSize" $0

  WriteUninstaller "$INSTDIR\uninstall.exe"
SectionEnd

Section "Sample config.yaml" SecConfig
  ; Seed a starter config.yaml only when none is already present, so upgrades
  ; preserve operator edits.
  ${IfNot} ${FileExists} "$ConfigPath"
    DetailPrint "Seeding $ConfigPath ..."
    SetOutPath "$INSTDIR"
    File /oname=config.yaml "..\config.example.yaml"
  ${Else}
    DetailPrint "Existing $ConfigPath preserved."
  ${EndIf}
SectionEnd

Section "Windows Firewall: inbound SMTP" SecFirewall
  DetailPrint "Adding firewall rule for TCP/${DEFAULT_SMTP_PORT}..."
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="${APP_NAME} inbound SMTP"'
  Pop $0
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="${APP_NAME} inbound SMTP" dir=in action=allow protocol=TCP localport=${DEFAULT_SMTP_PORT}'
  Pop $0
SectionEnd

;------------------------------------------------------------------------------
; Component descriptions
;------------------------------------------------------------------------------

LangString DESC_SecService  ${LANG_ENGLISH} "Install ${EXE_NAME} and register the Windows service. Required."
LangString DESC_SecConfig   ${LANG_ENGLISH} "Place a starter config.yaml in the install directory. Skipped if a config already exists."
LangString DESC_SecFirewall ${LANG_ENGLISH} "Allow inbound TCP/${DEFAULT_SMTP_PORT} so Exchange can relay outbound mail to the proxy."

!insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
  !insertmacro MUI_DESCRIPTION_TEXT ${SecService}  $(DESC_SecService)
  !insertmacro MUI_DESCRIPTION_TEXT ${SecConfig}   $(DESC_SecConfig)
  !insertmacro MUI_DESCRIPTION_TEXT ${SecFirewall} $(DESC_SecFirewall)
!insertmacro MUI_FUNCTION_DESCRIPTION_END

;------------------------------------------------------------------------------
; Finish-page hook
;------------------------------------------------------------------------------

Function OpenConfigInNotepad
  ${If} ${FileExists} "$ConfigPath"
    Exec 'notepad.exe "$ConfigPath"'
  ${EndIf}
FunctionEnd

;------------------------------------------------------------------------------
; Uninstaller
;------------------------------------------------------------------------------

Section "Uninstall"
  DetailPrint "Stopping service..."
  nsExec::ExecToLog 'sc.exe stop "${SERVICE_NAME}"'
  Pop $0
  Sleep 2000

  DetailPrint "Unregistering Windows service..."
  nsExec::ExecToLog '"$INSTDIR\${EXE_NAME}" uninstall'
  Pop $0

  DetailPrint "Removing firewall rule..."
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="${APP_NAME} inbound SMTP"'
  Pop $0

  Delete "$INSTDIR\${EXE_NAME}"
  Delete "$INSTDIR\uninstall.exe"

  ; Intentionally preserve $INSTDIR\config.yaml so operator edits are not
  ; destroyed by an uninstall. RMDir without /r only removes the directory
  ; when it is empty - so if the operator has deleted config.yaml beforehand,
  ; the install dir is cleaned up; otherwise it (and the config) survive.
  ${If} ${FileExists} "$INSTDIR\config.yaml"
    DetailPrint "Preserved $INSTDIR\config.yaml. Delete it manually if no longer needed."
  ${EndIf}
  RMDir "$INSTDIR"

  DeleteRegKey HKLM "${UNINSTALL_KEY}"
  DeleteRegKey HKLM "Software\${APP_NAME}"
SectionEnd
