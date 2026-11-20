# GSXComms parity and verification

The supplied Python v4.1 snapshot is the baseline. Mark each workflow as passed, failed, or unverified. Count a workflow toward the parity target only after its acceptance check passes.

| Workflow | Reference evidence | Implementation and verification |
|---|---|---|
| Startup and Settings | `main.py`, `ui_components.py` | Headless checks cover window layout, presets, sliders, immediate PTT and menu-reader saves, and initial creation of settings beside the EXE. Verify taskbar minimize to tray, restore, and layout at 150% DPI on clean Windows 11. |
| Accent Setup | `accent_setup.py`, `config_manager.py` | Check wizard save, dismissal, default selection, extraction beside the packaged archives, and playback from enabled packs. |
| Commands and menu recovery | `main.py`, `sim_bridge.py`, `words.json` | Differential and simulator fixtures cover routing, correction, preset order, menu changes, normalized numeric choices, and typed readout before transmission gates. Check reacquiring an idle overlay and the hotfix dialog in a live session. |
| MSFS 2024 and GSX transport | `sim_bridge.py`, `gsxremotecontrol.py` | FlyByWireless passed a live handshake, Couatl and aircraft reads, menu open, Additional Services selection, close, and remote-control release at KMCO Gate 34. Verify the main-window reconnect through a simulator restart. |
| MSFS 2020 and GSX transport | Same GSX LVars and panel file | Live verification pending. |
| PTT, crew audio and muting | `input_mapper.py`, `audio_engine.py`, `gsx_audio_muter.py` | Fixtures cover service state, gain, and the durable mute journal. Verify hardware bindings, non-default endpoints, recording static, and crash recovery with an actual Couatl session. |
| Recognizers and model management | `stt_engine.py`, `main.py` | Cached Parakeet and Orukeet passed worker restart tests. Existing fixtures cover resumed downloads. Verify fresh auto-download, cancel and retry, speech accuracy with representative recordings, and Voxtral with an authorized API key. |
| Kokoro | `kokoro_tts.py`, `phonemizer_engine.py` | The worker from the extracted ZIP synthesized menu text as 24 kHz PCM on CPU using the pinned model. Verify keyword readout in a live menu and CUDA with a compatible local runtime. |
| Distribution | `scripts/build.ps1`, `scripts/package.ps1` | The ZIP allowlist passed. The user selected an accent pack from a fresh extraction and reached the main window. Both packaged speech workers initialized. Clean Windows 11 coverage remains open. |

Launcher internals and performance benchmarking are outside this workflow checklist.
