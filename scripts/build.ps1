# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

param([string]$PhonemizerDir = 'build/phonemizer')
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
$repo = $PWD.Path
$PhonemizerDir = (Resolve-Path -LiteralPath $PhonemizerDir).Path
$tts = Join-Path $repo 'build/workers/kokoro'
New-Item -ItemType Directory -Path $tts -Force | Out-Null
$export = Get-Content -LiteralPath 'docs/phonemizer-export.json' -Raw | ConvertFrom-Json -AsHashtable
# The worker expects the tokenizer and graph exported from the same pinned checkpoint
foreach ($file in $export.files.Keys) {
    if ((Get-FileHash -LiteralPath (Join-Path $PhonemizerDir $file)).Hash -ne $export.files[$file]) {
        throw "Unexpected phonemizer export for $file"
    }
    Copy-Item -LiteralPath (Join-Path $PhonemizerDir $file) -Destination $tts -Force
}
# Fetch the package that carries the prebuilt x64 Windows runtime DLLs
$sherpa = go mod download -json 'github.com/k2-fsa/sherpa-onnx-go-windows/v2@v2.0.1' | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or !$sherpa.Dir) { throw 'sherpa-onnx module download failed' }
$modules = Join-Path $sherpa.Dir 'lib/x86_64-pc-windows-gnu'
$worker = Join-Path $repo 'build/workers/sherpa'
New-Item -ItemType Directory -Path $worker,(Join-Path $repo 'dist') -Force | Out-Null
$inventory = Get-Content -LiteralPath 'docs/license-inventory.json' -Raw | ConvertFrom-Json -AsHashtable
# Only inventory entries marked as native runtime files are copied from this package
$runtimeHashes = @{}
foreach ($entry in $inventory.components) {
    if ($entry.runtimeFile) { $runtimeHashes[$entry.runtimeFile] = $entry.sha256 }
}
# Verify the DLLs that will ship against the recorded redistribution inventory
foreach ($file in $runtimeHashes.Keys) {
    $source = Join-Path $modules $file
    if ((Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash -ne $runtimeHashes[$file]) {
        throw "Hash mismatch for $file"
    }
    Copy-Item -LiteralPath $source -Destination $worker -Force
}
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
$env:CGO_ENABLED = '1'
go build -trimpath -o (Join-Path $worker 'gsxspeech.exe') ./cmd/gsxspeech
if ($LASTEXITCODE -ne 0) { throw 'Speech worker build failed' }
# Each worker loads ONNX Runtime from its own executable directory
Copy-Item -LiteralPath (Join-Path $worker 'onnxruntime.dll') -Destination $tts -Force
# Worker launch always looks for gsxspeech.exe under the selected provider directory
go build -trimpath -o (Join-Path $tts 'gsxspeech.exe') ./cmd/gsxkokoro
if ($LASTEXITCODE -ne 0) { throw 'Kokoro worker build failed' }
# Native model libraries run in the workers, so the main executable can build without CGO
$env:CGO_ENABLED = '0'
# Use the patched Windows renderer only for the main application build
$modfile = & "$PSScriptRoot/go-gui-overlay.ps1"
go build -modfile $modfile -trimpath -ldflags '-H windowsgui' -o 'build/GSXComms.exe' ./cmd/gsxcomms
if ($LASTEXITCODE -ne 0) { throw 'Main executable build failed' }
go build -trimpath -o 'build/gsxremotecontrol.exe' ./cmd/gsxremotecontrol
if ($LASTEXITCODE -ne 0) { throw 'GSX console build failed' }
go build -trimpath -o 'build/gsxmute.exe' ./cmd/gsxmute
if ($LASTEXITCODE -ne 0) { throw 'GSX mute watchdog build failed' }
$env:CARGO_TARGET_X86_64_PC_WINDOWS_MSVC_RUSTFLAGS = '-C target-feature=+crt-static'
cargo +1.98.0 build --locked --release --manifest-path cmd/gsxsimconnect/Cargo.toml --target x86_64-pc-windows-msvc --target-dir build/rust
if ($LASTEXITCODE -ne 0) { throw 'SimConnect helper build failed' }
Copy-Item -LiteralPath 'build/rust/x86_64-pc-windows-msvc/release/gsxsimconnect.exe' -Destination 'build/gsxsimconnect.exe' -Force
& "$PSScriptRoot/notices.ps1" -Output 'build/THIRD_PARTY_NOTICES'
