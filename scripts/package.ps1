# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

param([string]$PackageName = 'GSXComms-Windows-x64')
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
$repo = $PWD.Path
$release = New-Item -ItemType Directory -Path 'dist/release' -Force
$zip = Join-Path $release.FullName "$PackageName.zip"
$partial = "$zip.partial"

$packs = @(Get-ChildItem -LiteralPath 'Accents' -File -Filter '*.zip')
if ($packs.Count -ne 8) { throw 'Expected all eight regional accent packs' }
foreach ($step in 1..5) {
    if (!(Test-Path -LiteralPath "tutorial/step$step.png" -PathType Leaf)) { throw "Missing tutorial/step$step.png" }
}
$files = @(
    Get-Item -LiteralPath 'LICENSE', 'build/THIRD_PARTY_NOTICES'
    Get-Item -LiteralPath 'build/GSXComms.exe', 'build/gsxsimconnect.exe', 'build/gsxmute.exe'
    Get-Item -LiteralPath 'build/workers/sherpa/gsxspeech.exe', 'build/workers/sherpa/sherpa-onnx-c-api.dll', 'build/workers/sherpa/onnxruntime.dll'
    Get-Item -LiteralPath 'build/workers/kokoro/gsxspeech.exe', 'build/workers/kokoro/onnxruntime.dll', 'build/workers/kokoro/phonemizer.onnx', 'build/workers/kokoro/phonemizer.json'
    $packs
    foreach ($step in 1..5) { Get-Item -LiteralPath "tutorial/step$step.png" }
)

$stream = [IO.File]::Create($partial)
try {
    $archive = [IO.Compression.ZipArchive]::new($stream, [IO.Compression.ZipArchiveMode]::Create)
    try {
        foreach ($file in $files) {
            $name = [IO.Path]::GetRelativePath($repo, $file.FullName).Replace('\', '/') -creplace '^build/', ''
            [void][IO.Compression.ZipFileExtensions]::CreateEntryFromFile($archive, $file.FullName, $name)
        }
    } finally { $archive.Dispose() }
} finally { $stream.Dispose() }

Move-Item -LiteralPath $partial -Destination $zip -Force
Get-Item -LiteralPath $zip
