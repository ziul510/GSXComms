# GSXComms review

## Summary

**Verified.** The first pass fixed ten issues across GSX menu selection and ownership, IPC availability, ground-speed validation, model recovery, legacy imports, and Voxtral response handling. The follow-up fixed credential recovery, named microphone handling in Voxtral mode, and IPC reconnection. `main` includes both passes through a fast-forward to `ff73bbc`. **Inferred.** Residual release risk remains moderate given incomplete native simulator, audio, credential, model, and installer verification.

## Scope and assumptions

- **Verified.** The review started at `4ae3a102` on Windows x64 with Go 1.27.0. Local `main` and the cached `origin/main` pointed to the same commit. The working tree was clean.
- **Inferred.** Treat behavior documented in the README, parity matrix, and existing tests as intended. Keep existing settings, defaults, and external interfaces unless a regression test demonstrates a defect.
- **Verified.** GitHub operations used Git and the workstation's credential helper. Both review passes were merged into `main` at the user's request.
- **Verified.** Scope covers the application and four Windows helpers, simulator and command processing, speech and audio, persistence and model management, data packaging, and build workflows. Generated model files, build output, and third-party code are excluded from source edits.

## Baseline evidence

- **Verified.** `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...`, tracked Go formatting, `go mod verify`, `CGO_ENABLED=0 go build ./...`, and the CI Windows software-renderer build passed.
- **Verified.** Live MSFS and installed speech-worker tests require opt-in environment variables and external fixtures. Both were disabled for this local review.
- **Verified.** A name-only Git history and tracked-source search found no matches for common private-key markers, GitHub credentials, AWS access-key IDs, or token prefixes. This is a limited pattern check.

## Changes and regression evidence

- **Verified, high confidence (98/100).** `356f2f6` fixes `internal/gsx/menu.go:209` (baseline). Disconnecting after only monitoring a simulator session wrote both close variables and could close another GSX menu. `TestClosePreservesUnownedMenu` observed both writes before the fix and zero after. `TestScriptedSession` still verifies release after a selection.
- **Verified, high confidence (98/100).** `4300997` fixes `internal/gsx/menu.go:223` (baseline). A zero-based index of 9 wrote a nonexistent tenth choice to the simulator transport. `TestChooseRejectsOutOfRangeMenuItems` observed the extra write before the fix. Afterward only the highest valid index, 8, was written.
- **Verified, high confidence (100/100).** `bce51af` fixes `internal/commands/commands.go:405` (baseline). Fuzzy menu matching read `option 10` as item 1 and `number 21` as item 2. `TestMatchRequiresWholeNumberTokens` reproduced both and checks valid whole-token matches. The Python reference corpus still passes.
- **Verified, high confidence (99/100).** `9a96bdd` fixes `internal/models/models.go:160` (baseline). A same-size corrupt Hugging Face cache file overwrote a partially downloaded model while retrying. `TestCacheImportPreservesResumableModel` observed a missing Range header before the fix. The fixed code resumes at byte 3 with corrupt cache and makes no HTTP request with verified cache. It rechecks the digest after copying to staging.
- **Verified, high confidence (97/100).** `afb0af7` fixes `cmd/gsxremotecontrol/main.go:65` (baseline). Starting IPC before SimConnect is reachable made the process exit on the third poll. `TestIPCKeepsPollingBeforeSimulatorStarts` reproduced the exit and now verifies retries, PING, and SHUTDOWN with a fake dialer. The compiled `gsxremotecontrol.exe --ipc` also failed before the fix and passed a five-second disconnected idle followed by PING, invalid SELECT, and SHUTDOWN after it.
- **Verified, high confidence (98/100).** `493933c` fixes `internal/gsx/menu.go:147` (baseline). NaN and infinity were accepted as valid ground speed. NaN and negative infinity bypassed the comparison in the standard transmission gate. `TestStatusRejectsNonFiniteGroundSpeed` observed successful reads before the fix and now checks that NaN and both infinities leave readiness intact while reporting speed unavailable.
- **Verified, high confidence (97/100).** `9cd32bc` fixes `cmd/gsxremotecontrol/main.go:88` (baseline). After a successful open write and an empty menu until timeout, IPC answered `menu_opened: true`. `TestIPCReportsMenuReadiness` reproduced it and verifies false on timeout and true with options.
- **Verified, high confidence (99/100).** `efa9219` fixes `internal/config/config.go:201` (baseline). Importing `[42]` replaced working accent mappings even though the runtime decoder could not use them. `TestImportRejectsMalformedAccentEntries` reproduced the overwrite and now checks both wrong entry and wrong field types. The compiled `gsxcomms.exe --import` rejected the synthetic fixture and preserved both destination files.
- **Verified, high confidence (99/100).** `f663f34` fixes `internal/speech/mistral.go:133` (baseline). HTTP 200 with object-valued transcription content was reported as silence. `TestVoxtralTranscriptionResponse` reproduced the silent failure with a fake transport and now checks the sanitized error. String, text-part arrays, null, and omitted content keep their previous behavior.
- **Verified, high confidence (96/100).** `0050a62` fixes `internal/gsx/menu.go:294–312` (baseline). When the first poll after selecting Additional Services still returned the parent, the command could choose the parent's Close option and fail to find the requested service. `TestExecuteWaitsForAdditionalServices` observed the unintended Close and now confirms selection in the refreshed submenu.

### Accent import brief

- **Verified problem.** Legacy import accepts malformed array entries and can replace usable overrides with unreadable data.
- **Verified evidence.** `internal/config/config.go:191–206` checks only the outer array type. `internal/geo/geo.go:127–131` requires typed entries.
- **Inferred approach.** A separate validation struct would allow the two paths to drift again. Share the decoder for the existing file format.
- **Inferred scope.** Import validation, embedded/loading calls for accent overrides, and the shared asset schema.
- **Verified evidence.** Existing valid migration and geolocation tests passed at baseline. The new test and CLI smoke check passed after the fix.

## Review coverage

| Area | Requirement assessment | Evidence |
| --- | --- | --- |
| Windows UI and PTT | **Inferred, partly met.** Controls, preferences, and command dispatch work in scripted paths. | **Verified.** Inspected state, queued saves, dialogs, preset order, keyboard, XInput, Raw Input, WinMM, and readout. Tests exercise control layout, save and rebind behavior, menu matching, and speed gates. |
| GSX and IPC | **Inferred, partly met.** Menu actions work for scripted input. Live behavior requires simulator coverage. | **Verified.** Traced GUI, IPC, console, and transport calls. Scripted session and disconnected executable checks passed. |
| Speech, cue playback, and mute recovery | **Inferred, partly met.** Frame handling, cloud gating, and recovery logic have local evidence. | **Verified.** Reviewed worker start, stop and retry, Voxtral request/error handling, PCM and ONNX inputs, and journal writes before a mute. Tests use synthetic PCM, mocked HTTP, and fake audio sessions. |
| Settings and models | **Inferred, partly met.** Local import, credential recovery, and model resume have evidence. Native vault behavior remains unverified. | **Verified.** Reviewed atomic file writes, model lock and cache publication, override parsing, credentials, and staged cleanup. Tests verify rollback, vault failure, and digest checks with local fixtures. |
| Vocabulary, geolocation, IPA, and assets | **Inferred, met for covered cases.** | **Verified.** Read the reference inputs and Go parsers. Differential matching, location fixtures, IPA, and bundled-cue tests passed. |
| Build and distribution | **Inferred, partly met.** Executable builds have evidence. Packaged release behavior needs the pinned installer inputs. | **Verified.** Reviewed workflows, dependency inventory, renderer overlay, NSIS lifecycle, and asset generation scripts. Compiled both normal and software-rendered builds. |

## Findings left open

- **Verified parser behavior, inferred live trigger, 75/100.** `internal/gsx/menu.go:49,64` strips the leading digits of unnumbered labels such as `45° left pushback`. A captured simulator menu with a numeric label would establish the supported grammar and provide a fixture for a safe change.
- **Verified behavior, inferred policy, 75/100.** `internal/app/views.go:163` calls `choose` directly. Typed commands and PTT pass through `transmissionBlock` in `internal/app/app.go:420–448`. A fake simulator click test can pin whether manual overlay selections are intended to bypass the transmission speed policy.

## Hardcodes and credentials

- **Verified.** Named the nine-item GSX selection bound in the session code and reused it in parsing and selection. The boundary test covers the ninth item and the adjacent invalid index.
- **Verified.** Moved the accent mappings filename and decoder to the asset package. The file format, embedded data, import format, and existing settings keys retain compatibility.
- **Verified.** Configuration already keeps user defaults in `internal/config/defaults.json`, model revisions and digests in `internal/models/manifest.json`, and vocabulary and geolocation in asset data. Existing SDK, DLL, model-cache, and panel path overrides are optional.
- **Inferred.** Vendor hosts, Windows API constants, PCM formats, and model tensor sizes are protocol or provider identities. Moving provider hosts into environment settings would change the destination for credentials or model downloads. Existing workflow pins, retry and settle timing, queue sizes, the 50-knot limit, and recording bounds were reviewed and kept at current values. No new environment or configuration keys.
- **Verified.** The application stores Voxtral credentials in Windows Credential Manager. The reviewed history and tracked files produced no rotation candidates in the limited pattern scan. Scanner coverage limits are listed below.

## Checks and remaining gaps

- **Verified.** Each fix passed its focused test, uncached `go test -race -count=1 ./...`, `go vet ./...`, tracked Go formatting, normal and software-renderer builds, and `git diff --check`. The fixes preserve the existing tests without changing their assertions.
- **Verified.** Final uncached tests and race tests passed. Vet, tracked-file `gofmt -l`, `go mod verify`, and `git diff --check 4ae3a102..HEAD` passed. `go build ./...` passed with CGO enabled and disabled. With CGO disabled, `go build -modfile <path returned by scripts/go-gui-overlay.ps1> ./...` passed.
- **Verified.** The final Git scan found no credential pattern signals in reachable history, tracked files, or this report. The disconnected IPC and malformed import executable checks passed again after the final code commit.
- **Verified.** The local source inventory and normalized 12-line duplicate check found zero cross-file windows at that threshold. Numeric-literal searches supplied review leads for protocol values, local tuning, and user defaults.
- **Verified.** Gitleaks, TruffleHog, Semgrep, and jscpd were unavailable locally. The offline Staticcheck attempt lacked cached Go dependencies. The vulnerability scan lacked an offline advisory database. The broader shell scanner setup was rejected by command policy, so the review used a redacted, name-only Git pattern search.
- **Verified.** Live MSFS, real Credential Manager operations, microphone and Core Audio behavior, authenticated cloud calls, installed speech workers, GPU inference, and the unsigned installer were outside the available local fixtures. NSIS 3.12 and the pinned phonemizer export were absent. These paths remain unverified.

## Follow-up credential recovery brief

- **Verified problem.** A failed startup migration can be followed by a successful settings write that strips the only copy of a legacy credential.
- **Verified evidence.** `internal/config/config.go:112–124,218–232,257–263` and `internal/app/local_io.go:27–32`. A credential-free import can also replace the destination settings file.
- **Inferred approach.** Protect the settings write and credential-free import paths together. Handling the startup error alone would leave both replacement paths exposed.
- **Inferred scope.** Implicit migration uses an existing vault entry if present. An explicit import that supplies a credential retains its replacement behavior.
- **Verified baseline.** The full local race suite and vet passed at `8685ad9`. Synthetic file fixtures and a fake vault exercise recovery without touching the workstation's credentials.

## Follow-up changes and regression evidence

- **Verified, 100/100.** `f9b88b8` fixes legacy credential loss on settings replacement in `internal/config/config.go:112–124,218–232,257–263` at `8685ad9`. `TestSettingsSavePreservesLegacyCredentialUntilMigrated` and `TestImportWithoutCredentialPreservesUnmigratedDestination` both reproduced overwritten files when the vault was unavailable. Afterward both preserved the files, and the save retry persisted the requested preference and removed the plaintext entry. `TestImplicitMigrationKeepsExistingVaultCredential` verifies that automatic migration preserves an existing vault entry. An explicit import with its own credential retains replacement and rollback behavior.
- **Verified, 100/100.** `a14f154` fixes the native capture fallback in `internal/app/audio_windows.go:53–63,91–96` at `8685ad9`. `TestOpenCaptureDeviceFallback` reproduced a default-device open for missing and busy named microphones before the fix. Voxtral now stops in both cases. The test covers explicit default selection, legacy blank settings, and local recognition fallback. **Inferred policy.** Selecting a named microphone for cloud recognition limits capture to that device.
- **Verified, 100/100.** `d48c438` fixes the IPC loop in `cmd/gsxremotecontrol/main.go:74–81` at `8685ad9`. `TestIPCReconnectsAfterEstablishedTransportFails` saw IPC exit after three failed health checks. It now verifies that IPC closes the old transport, connects to a replacement, and answers PING on the same stream. The real executable also passed the disconnected idle and command smoke check. **Inferred policy.** The reconnect workflow in the parity matrix applies to established connections as well as initial startup.
- **Verified.** Focused tests, the full race suite, vet, formatting, and Windows builds passed after each fix. The final pass also ran uncached ordinary tests, `go mod verify`, both CLI smoke checks, and the name-only Git credential scan with no matches. The Windows audio and vault tests used fake devices and synthetic credentials. Live hardware, cloud, and packaging checks and dedicated secret scanning remain unverified.
