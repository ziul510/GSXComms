# GSXComms

## Install and start

Extract the Windows x64 ZIP into a folder you can write to, then run **GSXComms.exe**. The folder holds `user_settings.json`, downloaded models under `models`, and extracted voice packs under `Accents`. Optional `words.json` and `accent_mappings.json` files in this folder override the bundled defaults. Use `--data-dir` to specify a different folder for settings and runtime files.

The selected local speech model downloads and loads in the background at startup. **Settings** can retry a missing download or switch to an installed model. Enable the menu reader to download Kokoro. Choose **Set up Mistral** for the illustrated API key tutorial. Validating your key selects Mistral and stores the key in Windows Credential Manager. Mistral receives eligible voice recordings and may use them to train AI models, depending on your plan and privacy settings. The default PTT key is M. If a named microphone is unavailable in Mistral mode, recording stops. Choose **Default** under **Microphone** to use the Windows default device.

On first launch, **Accent Setup** opens before the main window. Select at least one regional pack and a default accent, then save. Closing the wizard defers setup. You can reopen it from Settings. Deselected packs stay on disk for reuse. The recordings were generated with ElevenLabs. Crew voices use aircraft location and fall back to your default or another installed accent. **Force accent** applies to the next crew interaction.

## Build

Use Windows x64, Go 1.27.0, a MinGW-w64 UCRT C compiler, PowerShell 7, and Rust 1.98.0 with the MSVC x64 build tools. The builder prepares the pinned go-gui software renderer, verifies the bundled sherpa-onnx DLLs, and builds the FlyByWireless SimConnect helper.

For Kokoro, download the pinned checkpoint listed in [the export manifest](docs/phonemizer-export.json). In a Python 3.12 build environment with the listed dependencies, export the phonemizer. Setup verifies the graph and tokenizer hashes. Python and the build tools are unnecessary on end-user PCs.

```powershell
python scripts/export-phonemizer.py C:\models\best_model.pt build\phonemizer
go vet ./...
go build ./...
rustup toolchain install 1.98.0 --profile minimal --target x86_64-pc-windows-msvc
pwsh -File scripts/build.ps1 -PhonemizerDir build\phonemizer
pwsh -File scripts/package.ps1
```

The portable package is `dist/release/GSXComms-Windows-x64.zip`.

## GitHub releases

Publish a GitHub Release for a tag that includes the release workflow. **Build Windows release** attaches the portable ZIP with GSXComms.exe, the required runtime files, the Accents and tutorial folders, LICENSE, and third-party notices.

Run **Build Windows release** manually from Actions to get the same files as a workflow artifact.

## Verify live GSX

Start a flight in MSFS at a GSX airport. The bundled FlyByWireless helper connects through the simulator's local Windows named pipe.

Verify the GSX readiness badge, open the menu, select **Additional Services** by its spoken or typed number, and close the menu. With **Auto-connect to simulator** enabled, GSXComms reconnects after a simulator restart. Developers can run `build/gsxremotecontrol.exe` with `m` to toggle the menu, `1`–`9` to select, `0` to close, and `q` to quit. Use `--ipc` for the line-delimited interface.

See the [parity matrix](docs/parity.md) and [license inventory](docs/license-inventory.json).

Copyright 2026 ziul510. The rewrite code is Apache-2.0. Third-party components retain their own terms in `THIRD_PARTY_NOTICES`.
