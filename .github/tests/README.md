# On-demand tests

Run from the repository root on Windows with PowerShell 7 and the Go build prerequisites.

```powershell
pwsh -File .github/tests/run.ps1
```

The runner uses a temporary Go overlay to run the tests in their original packages. It enables race detection and leaves the working tree untouched. The simulator and installed speech-worker tests still require their opt-in environment variables.
