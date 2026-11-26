# On-demand tests

Run from the repository root on Windows with PowerShell 7 and the Go build prerequisites.

```powershell
pwsh -File .github/tests/run.ps1
```

The runner uses a temporary Go overlay to run the tests in their original packages. It enables race detection and leaves the working tree untouched. The simulator and installed speech-worker tests still require their opt-in environment variables.

Set `GSXCOMMS_TEST_ASR_MODEL` to the absolute path to a verified Parakeet or Orukeet model directory. The restart test uses `build/workers/sherpa/gsxspeech.exe` with its adjacent runtime DLLs. Set `GSXCOMMS_TEST_ASR_WORKER` to an absolute executable path to test another installation.

Set `GSXCOMMS_TEST_LIVE_WINDOWS_UI=1` on an interactive Windows desktop to check dialog placement against the first rendered layout. Run once with `GOGUI_RENDERER=gl` and once with `GOGUI_RENDERER=soft` to cover both renderers.
