# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '../..')).Path
$replace = @{}
foreach ($test in Get-ChildItem -LiteralPath $PSScriptRoot -Recurse -File -Filter '*_test.go') {
    $path = [IO.Path]::GetRelativePath($PSScriptRoot, $test.FullName)
    $replace[(Join-Path $repo $path)] = $test.FullName
}
if (!$replace.Count) { throw 'No Go tests found' }

$overlay = [IO.Path]::GetTempFileName()
Push-Location -LiteralPath $repo
try {
    @{ Replace = $replace } | ConvertTo-Json | Set-Content -LiteralPath $overlay -Encoding utf8
    go test -race -count=1 "-overlay=$overlay" ./...
    exit $LASTEXITCODE
} finally {
    Pop-Location
    [IO.File]::Delete($overlay)
}
