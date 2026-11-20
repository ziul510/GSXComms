# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

$ErrorActionPreference = 'Stop'
# Compute overlay paths from the repository root regardless of where the runner starts.
$repo = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '../..')).Path
$replace = @{}
foreach ($test in Get-ChildItem -LiteralPath $PSScriptRoot -Recurse -File -Filter '*_test.go') {
    # Tests need their original package paths to reach unexported code.
    $path = [IO.Path]::GetRelativePath($PSScriptRoot, $test.FullName)
    $replace[(Join-Path $repo $path)] = $test.FullName
}
if (!$replace.Count) { throw 'No Go tests found' }

# Go can load those package paths without copying test files into the source tree.
$overlay = [IO.Path]::GetTempFileName()
Push-Location -LiteralPath $repo
try {
    @{ Replace = $replace } | ConvertTo-Json | Set-Content -LiteralPath $overlay -Encoding utf8
    # Run every package under the race detector without reusing a prior test result.
    go test -race -count=1 "-overlay=$overlay" ./...
    exit $LASTEXITCODE
# Remove the temporary overlay even if a test fails.
} finally {
    Pop-Location
    [IO.File]::Delete($overlay)
}
