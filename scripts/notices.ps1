# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

param([string]$Output = 'THIRD_PARTY_NOTICES', [string]$NSISRoot)
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
$inventory = Get-Content -LiteralPath 'docs/license-inventory.json' -Raw | ConvertFrom-Json -AsHashtable
$notice = [Text.StringBuilder]::new()
function Add-Notice([string]$text) { [void]$notice.AppendLine($text) }
Add-Notice 'Third-party notices for GSXComms'
Add-Notice 'Copyright 2026 ziul510. Apache License 2.0 applies to the rewrite code.'
Add-Notice ''
foreach ($entry in $inventory.components) {
    Add-Notice "$($entry.name) $($entry.version) — $($entry.license)"
    Add-Notice "Use: $($entry.mode)"
    Add-Notice "Source: $($entry.source)"
    if ($entry.terms) { Add-Notice "Terms: $($entry.terms)" }
    if ($entry.alternative) { Add-Notice "Alternative: $($entry.alternative)" }
    Add-Notice ''
}
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
$env:CGO_ENABLED = '1'
$modules = go list -deps -f '{{with .Module}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}' ./cmd/... |
    Where-Object { $_ -and $_ -notmatch '^github.com/ziul510/GSXCommsGo\|' } | Sort-Object -Unique
if ($LASTEXITCODE -ne 0) { throw 'Failed to inventory linked modules' }
foreach ($line in $modules) {
    $path,$version,$dir = $line -split '\|'
    $approved = $inventory.modules[$path]
    if (!$approved -or $approved.version -ne $version) { throw "Unreviewed linked module $path $version" }
    Add-Notice "========== $path $version ($($approved.license)) =========="
    foreach ($license in $approved.files) {
        Add-Notice (Get-Content -LiteralPath (Join-Path $dir $license) -Raw)
    }
}
Add-Notice '========== Go standard library and runtime =========='
Add-Notice (Get-Content -LiteralPath (Join-Path (go env GOROOT) 'LICENSE') -Raw)
$sherpa = go mod download -json 'github.com/k2-fsa/sherpa-onnx-go-windows/v2@v2.0.1' | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or !$sherpa.Dir) { throw 'Could not find sherpa-onnx license' }
Add-Notice '========== sherpa-onnx Windows =========='
Add-Notice (Get-Content -LiteralPath (Join-Path $sherpa.Dir 'LICENSE') -Raw)
foreach ($file in (Get-ChildItem 'docs/licenses' -File | Sort-Object Name)) {
    Add-Notice "========== $($file.Name) =========="
    Add-Notice (Get-Content -LiteralPath $file.FullName -Raw)
}
if ($NSISRoot) {
    Add-Notice '========== NSIS installer =========='
    Add-Notice (Get-Content -LiteralPath (Join-Path $NSISRoot 'COPYING') -Raw)
}
Set-Content -LiteralPath $Output -Value $notice.ToString() -Encoding utf8
