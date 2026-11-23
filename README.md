# GSXComms

GSXComms is a Windows app for controlling GSX ground services in Microsoft Flight Simulator. You can request services by voice or keyboard, hear crew replies in regional accents, and have the GSX menu read aloud.

## Get started

1. Download `GSXComms-Windows-x64.zip` from [GitHub Releases](https://github.com/ziul510/GSXComms/releases), extract it to a writable folder, and run `GSXComms.exe`.
2. In **Accent Setup**, select at least one voice pack and click **Save**. Pick a **Fallback Accent** and click **Continue**. GSXComms uses an installed accent that matches the aircraft's location when available.
3. Start a flight at an airport and wait for GSXComms to connect, or click **Connect**. Say the service you want to execute or the number of the action in the GSX menu. To type a choice, turn on **Enable keyboard typing** in **Settings**.

## Speech and settings

GSXComms uses Orukeet by default and downloads the selected local speech model on startup if it isn't installed. Use **Settings** to switch models, change the push-to-talk binding, or enable the Kokoro menu reader.

For cloud recognition, choose the **Mistral** provider in settings and follow the API key setup. GSXComms stores the key in Windows Credential Manager and sends recordings with detected speech to Mistral for transcription. Mistral may use those recordings for training depending on your plan and privacy settings.

GSXComms keeps `user_settings.json`, downloaded models, and extracted voice packs beside the executable by default. Use `--data-dir` to change that folder. Place `words.json` or `accent_mappings.json` there to override the bundled defaults. Use a separate data directory for each running instance.

GSXComms encrypts new corrupt settings backups for the Windows user who created them. Older `user_settings.json.corrupt.*` backups without the `.dpapi` extension can contain a legacy Mistral API key. If you have one of those backups, rotate that key in Mistral and remove the backup when you no longer need it.

## Build from source

On Windows x64, use Go 1.27.4, PowerShell 7, a MinGW-w64 UCRT C compiler, and Rust 1.98.0 with the MSVC x64 build tools. For the Kokoro phonemizer export, use Python 3.12 and the checkpoint and dependencies in the [export manifest](docs/phonemizer-export.json).

```powershell
py -3.12 -m pip install --require-hashes -r docs/phonemizer-build-requirements.txt
py -3.12 -m pip install --require-hashes --no-build-isolation -r docs/phonemizer-requirements.txt
py -3.12 scripts/export-phonemizer.py C:\models\best_model.pt build\phonemizer
rustup toolchain install 1.98.0 --profile minimal --target x86_64-pc-windows-msvc
pwsh -File scripts/build.ps1 -PhonemizerDir build\phonemizer
pwsh -File scripts/package.ps1
```

Find the ZIP at `dist/release/GSXComms-Windows-x64.zip`. You can also download it as an artifact after running the [Build Windows release workflow](.github/workflows/build.yml) manually. See the [verification matrix](docs/parity.md) for simulator test coverage.

## License

Copyright 2026 ziul510. GSXComms code is licensed under [Apache 2.0](LICENSE). See [third-party notices](THIRD_PARTY_NOTICES) for component licenses.
