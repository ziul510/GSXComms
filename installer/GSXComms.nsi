; Copyright 2026 ziul510
; SPDX-License-Identifier: Apache-2.0
Unicode True
; Setup uses the current user's application directory and registry hive
RequestExecutionLevel user
SetCompressor /SOLID zlib
Name "GSXComms (Test Build)"
Caption "GSXComms Setup"
OutFile "..\dist\GSXCommsGo-win-x64-unsigned-test.exe"
InstallDir "$LOCALAPPDATA\Programs\GSXCommsGo"
Icon "..\internal\assets\gsxcommsThanksToAviationist101.ico"

!define UNINSTALL "Software\Microsoft\Windows\CurrentVersion\Uninstall\GSXCommsGo"
!include "MUI2.nsh"
!include "x64.nsh"
!define MUI_LICENSEPAGE_TEXT_TOP "GSXComms license and third-party notices"
!insertmacro MUI_PAGE_LICENSE "..\build\setup-notices.txt"
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

!macro CheckAvailable file
  ${If} ${FileExists} "$INSTDIR\${file}"
    ClearErrors
    ; Append mode probes for a write lock without truncating the binary
    FileOpen $0 "$INSTDIR\${file}" a
    ${If} ${Errors}
      MessageBox MB_OK|MB_ICONSTOP "Setup cannot update ${file}. Close GSXComms and try again." /SD IDOK
      SetErrorLevel 2
      Abort
    ${EndIf}
    FileClose $0
  ${EndIf}
!macroend

!macro Asset action folder file
  !if "${action}" == "install"
    SetOutPath "$INSTDIR\${folder}"
    File "..\${folder}\${file}"
  !else
    Delete "$INSTDIR\${folder}\${file}"
  !endif
!macroend

!macro TutorialAndAccents action
  !insertmacro Asset "${action}" "Accents" "East Asia & Pacific.zip"
  !insertmacro Asset "${action}" "Accents" "Eastern Europe.zip"
  !insertmacro Asset "${action}" "Accents" "English-Speaking Countries.zip"
  !insertmacro Asset "${action}" "Accents" "Latin & Southern Europe.zip"
  !insertmacro Asset "${action}" "Accents" "Middle East & North Africa.zip"
  !insertmacro Asset "${action}" "Accents" "South & Southeast Asia.zip"
  !insertmacro Asset "${action}" "Accents" "Sub-Saharan Africa.zip"
  !insertmacro Asset "${action}" "Accents" "Western & Northern Europe.zip"
  !insertmacro Asset "${action}" "tutorial" "step1.png"
  !insertmacro Asset "${action}" "tutorial" "step2.png"
  !insertmacro Asset "${action}" "tutorial" "step3.png"
  !insertmacro Asset "${action}" "tutorial" "step4.png"
  !insertmacro Asset "${action}" "tutorial" "step5.png"
!macroend

; Check every bundled executable and runtime before replacing or removing any of them
!macro CheckProgramFiles
  !insertmacro CheckAvailable "GSXComms.exe"
  !insertmacro CheckAvailable "gsxremotecontrol.exe"
  !insertmacro CheckAvailable "gsxmute.exe"
  !insertmacro CheckAvailable "workers\sherpa\gsxspeech.exe"
  !insertmacro CheckAvailable "workers\sherpa\sherpa-onnx-c-api.dll"
  !insertmacro CheckAvailable "workers\sherpa\onnxruntime.dll"
  !insertmacro CheckAvailable "workers\kokoro\gsxspeech.exe"
  !insertmacro CheckAvailable "workers\kokoro\onnxruntime.dll"
!macroend

; The packaged Windows binaries are all x64
Function .onInit
  ${IfNot} ${RunningX64}
    MessageBox MB_ICONSTOP "GSXComms requires 64-bit Windows."
    Abort
  ${EndIf}
FunctionEnd

Section "GSXComms"
  SetShellVarContext current
  ; Abort before copying if the running app or a worker has a file locked
  !insertmacro CheckProgramFiles
  SetOutPath "$INSTDIR"
  File "..\build\GSXComms.exe"
  File "..\build\gsxremotecontrol.exe"
  File "..\build\gsxmute.exe"
  File "..\LICENSE"
  File "..\THIRD_PARTY_NOTICES"
  !insertmacro TutorialAndAccents "install"
  ; Both workers use the same executable name but keep separate DLL directories
  SetOutPath "$INSTDIR\workers\sherpa"
  File "..\build\workers\sherpa\gsxspeech.exe"
  File "..\build\workers\sherpa\sherpa-onnx-c-api.dll"
  File "..\build\workers\sherpa\onnxruntime.dll"
  SetOutPath "$INSTDIR\workers\kokoro"
  File "..\build\workers\kokoro\gsxspeech.exe"
  File "..\build\workers\kokoro\onnxruntime.dll"
  File "..\build\workers\kokoro\phonemizer.onnx"
  File "..\build\workers\kokoro\phonemizer.json"
  SetOutPath "$INSTDIR"
  ; Register the uninstaller with the current user's Apps list
  WriteUninstaller "$INSTDIR\Uninstall.exe"
  CreateDirectory "$SMPROGRAMS\GSXComms Go"
  CreateShortcut "$SMPROGRAMS\GSXComms Go\GSXComms.lnk" "$INSTDIR\GSXComms.exe"
  WriteRegStr HKCU "${UNINSTALL}" "DisplayName" "GSXComms (Test Build)"
  ; Clear the display version left behind by earlier installers
  ClearErrors
  DeleteRegValue HKCU "${UNINSTALL}" "DisplayVersion"
  ClearErrors
  WriteRegStr HKCU "${UNINSTALL}" "Publisher" "ziul510"
  WriteRegStr HKCU "${UNINSTALL}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINSTALL}" "UninstallString" '$\"$INSTDIR\Uninstall.exe$\"'
  WriteRegDWORD HKCU "${UNINSTALL}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTALL}" "NoRepair" 1
SectionEnd

Section "Uninstall"
  SetShellVarContext current
  ReadRegStr $1 HKCU "${UNINSTALL}" "InstallLocation"
  ${If} $1 == ""
    ; Require app-specific files before trusting a path with no registry entry
    ${If} ${FileExists} "$INSTDIR\gsxremotecontrol.exe"
    ${AndIf} ${FileExists} "$INSTDIR\workers\sherpa\sherpa-onnx-c-api.dll"
      StrCpy $1 $INSTDIR
    ${EndIf}
  ${EndIf}
  ; Restrict removal to the installation this uninstaller belongs to
  ${If} $1 != $INSTDIR
    MessageBox MB_OK|MB_ICONSTOP "Run the uninstaller from the registered GSXComms installation." /SD IDOK
    SetErrorLevel 2
    Abort
  ${EndIf}
  !insertmacro CheckProgramFiles
  ; Remove only files owned by this installer Settings and downloads live outside the program directory
  ClearErrors
  Delete "$INSTDIR\workers\sherpa\onnxruntime.dll"
  Delete "$INSTDIR\workers\sherpa\sherpa-onnx-c-api.dll"
  Delete "$INSTDIR\workers\sherpa\gsxspeech.exe"

  Delete "$INSTDIR\workers\kokoro\gsxspeech.exe"
  Delete "$INSTDIR\workers\kokoro\onnxruntime.dll"
  Delete "$INSTDIR\workers\kokoro\phonemizer.onnx"
  Delete "$INSTDIR\workers\kokoro\phonemizer.json"

  Delete "$INSTDIR\GSXComms.exe"
  Delete "$INSTDIR\gsxremotecontrol.exe"
  Delete "$INSTDIR\gsxmute.exe"
  Delete "$INSTDIR\LICENSE"
  Delete "$INSTDIR\THIRD_PARTY_NOTICES"
  !insertmacro TutorialAndAccents "uninstall"
  ; Leave the installer registration available for another attempt if file removal fails
  ${If} ${Errors}
    MessageBox MB_OK|MB_ICONSTOP "Some program files could not be removed. Close GSXComms and run uninstall again." /SD IDOK
    SetErrorLevel 2
    Abort
  ${EndIf}
  ; Remove only empty directories so extra files under the install path survive
  RMDir "$INSTDIR\workers\sherpa"
  RMDir "$INSTDIR\workers\kokoro"
  RMDir "$INSTDIR\workers"
  RMDir "$INSTDIR\Accents"
  RMDir "$INSTDIR\tutorial"
  Delete "$SMPROGRAMS\GSXComms Go\GSXComms.lnk"
  RMDir "$SMPROGRAMS\GSXComms Go"
  DeleteRegKey HKCU "${UNINSTALL}"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"
SectionEnd
