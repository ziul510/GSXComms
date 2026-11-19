# GSXComms

## Install and start

Run the x64 installer, then start **GSXComms** from **GSXComms Go** in the Start menu. Setup runs per user. Settings and models live in `%LOCALAPPDATA%\GSXCommsGo`. Uninstall removes the program and shortcut and preserves user data. Offline install, launch and uninstall passed on a Windows 10 Sandbox with graphics acceleration disabled. Signing and Windows 11 verification remain open.

In **Settings**, open **Speech model setup** to download or reload a model. The default PTT key is M. For Voxtral, validate a key in the speech dialog and choose the Mistral provider. Windows Credential Manager stores the key. Only eligible recordings in Voxtral mode go to Mistral. If a named microphone is unavailable in Voxtral mode, recording stops. Choose **Default** under **Microphone** to use the Windows default device.

## Import legacy settings

Close the Go app before importing. Pass `--import` with the path to the old `GSXCommsFiles` directory. This replaces the Go app's settings with migrated values, copies vocabulary and accent mapping overrides, and preserves the legacy directory. Choose **Download** in Settings to reuse matching cached model files.

```powershell
& "$env:LOCALAPPDATA\Programs\GSXCommsGo\GSXComms.exe" --import "C:\path\to\GSXCommsFiles"
```

## Build

Use Windows x64, Go 1.27.0, a MinGW-w64 UCRT C compiler, PowerShell 7, and NSIS 3.12 on the build machine. The builder prepares the pinned go-gui source with the Windows software renderer and verifies the bundled sherpa-onnx Windows DLLs.

For Kokoro, download the pinned checkpoint listed in [the export manifest](docs/phonemizer-export.json). In a Python 3.12 build environment with the listed dependencies, export the phonemizer. Setup verifies the graph and tokenizer hashes. Python and the build tools are unnecessary on end-user PCs.

```powershell
python scripts/export-phonemizer.py C:\models\best_model.pt build\phonemizer
go vet ./...
go build ./...
pwsh -File scripts/build.ps1 -MakeNSIS C:\tools\nsis\makensis.exe -PhonemizerDir build\phonemizer
```

The unsigned package is written to `dist`.

## GitHub releases

Publish a GitHub Release for a tag that includes the release workflow. **Build Windows release** builds the unsigned x64 installer and attaches the executable, ZIP, and ZIP SHA-256 checksum. The ZIP also contains the license, third-party notices, and a checksum file for its contents.

Run **Build Windows release** manually from Actions to get the same files as a workflow artifact.

## Verify live GSX

Start a flight in MSFS at a GSX airport. GSXComms discovers an existing native SimConnect client through common SDK paths, the GSX installation, and the compatible copy already installed with Lorby AxisAndOhs for MSFS 2024. Set `GSX_SIMCONNECT_DLL` to select another compatible vendor DLL already on the PC. Microsoft redistribution permission for a bundled client remains open.

Connect, verify the GSX readiness badge, open the menu, select **Additional Services**, close the menu, and reconnect. The standalone console supports `m` to toggle the menu, `1`–`9` to select, `0` to close, and `q` to quit. Use `--ipc` for the line-delimited interface. IPC stays available while it retries a lost simulator connection.

See the [parity matrix](docs/parity.md) and [license inventory](docs/license-inventory.json).

Copyright 2026 ziul510. The rewrite code is Apache-2.0. Third-party components retain their own terms in `THIRD_PARTY_NOTICES`.
