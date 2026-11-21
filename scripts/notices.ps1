# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

param([string]$Output = 'THIRD_PARTY_NOTICES', [string]$ModuleReport)
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
$inventory = Get-Content -LiteralPath 'docs/license-inventory.json' -Raw | ConvertFrom-Json -AsHashtable
$notice = [Text.StringBuilder]::new()
# Build one notice file without emitting a pipeline result for each appended line
function Add-Notice([string]$text) { [void]$notice.AppendLine($text) }
Add-Notice 'Third-party notices for GSXComms'
Add-Notice 'Copyright 2026 ziul510. Apache License 2.0 applies to the rewrite code.'
Add-Notice ''
# Include separately shipped data, libraries, and assets that Go cannot inventory
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
# Include dependencies linked by the native speech workers in the inventory
$env:CGO_ENABLED = '1'
# Inventory only modules linked by the command binaries
$modules = go list -deps -f '{{with .Module}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}' ./cmd/... |
    Where-Object { $_ -and $_ -notmatch '^github.com/ziul510/GSXCommsGo\|' } | Sort-Object -Unique
if ($LASTEXITCODE -ne 0) { throw 'Failed to inventory linked modules' }
$report = [Collections.Generic.List[string]]::new()
$report.Add('Module,Version,License')
foreach ($line in $modules) {
    $path,$version,$dir = $line -split '\|'
    $approved = $inventory.modules[$path]
    # A version change requires a fresh license review before packaging
    if (!$approved -or $approved.version -ne $version) { throw "Unreviewed linked module $path $version" }
    $report.Add("$path,$version,$($approved.license)")
    Add-Notice "========== $path $version ($($approved.license)) =========="
    # Include each reviewed license from the exact version selected by Go
    foreach ($license in $approved.files) {
        Add-Notice (Get-Content -LiteralPath (Join-Path $dir $license) -Raw)
    }
}
Add-Notice '========== Go standard library and runtime =========='
Add-Notice (Get-Content -LiteralPath (Join-Path (go env GOROOT) 'LICENSE') -Raw)
# The Sherpa DLL is loaded at runtime and won't show up in linked Go modules
$sherpa = go mod download -json 'github.com/k2-fsa/sherpa-onnx-go-windows/v2@v2.0.1' | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or !$sherpa.Dir) { throw 'Could not find sherpa-onnx license' }
Add-Notice '========== sherpa-onnx Windows =========='
Add-Notice (Get-Content -LiteralPath (Join-Path $sherpa.Dir 'LICENSE') -Raw)
foreach ($file in (Get-ChildItem 'docs/licenses' -File | Sort-Object Name)) {
    Add-Notice "========== $($file.Name) =========="
    Add-Notice (Get-Content -LiteralPath $file.FullName -Raw)
}
$rust = cargo +1.98.0 metadata --locked --manifest-path cmd/gsxsimconnect/Cargo.toml --filter-platform x86_64-pc-windows-msvc --format-version 1 | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or !$rust.packages) { throw 'Failed to inventory Rust dependencies' }
foreach ($crate in ($rust.packages | Where-Object source | Sort-Object name)) {
    $license = $crate.license
    if (!$license -and $crate.license_file) {
        $license = (Import-Csv -LiteralPath 'third_party/licenses.csv' | Where-Object {
            $_.Ecosystem -eq 'Rust' -and $_.Dependency -eq $crate.name -and $_.Version -eq $crate.version
        }).License
    }
    if (!$license) { throw "Missing Rust license identifier for $($crate.name) $($crate.version)" }
    Add-Notice "========== $($crate.name) $($crate.version) ($license) =========="
    $crateDir = Split-Path -Parent $crate.manifest_path
    $licenses = @(Get-ChildItem -LiteralPath $crateDir -File | Where-Object {
        $_.Name -match '^(licen[cs]e|unlicen[cs]e|copying|notice|patents|third.party.notices)($|[._-])'
    } | Select-Object -ExpandProperty FullName)
    if ($crate.license_file) { $licenses += (Get-Item -LiteralPath (Join-Path $crateDir $crate.license_file)).FullName }
    if (!$licenses.Count) {
        if ($crate.name -notmatch '^flybywireless-simconnect(-derive|-proto)?$' -or
            $crate.version -ne '0.2.1' -or $license -ne 'MIT') {
            throw "Missing Rust license for $($crate.name) $($crate.version)"
        }
        Add-Notice 'See FlyByWireless-LICENSE above.'
    }
    foreach ($file in ($licenses | Sort-Object -Unique)) {
        Add-Notice (Get-Content -LiteralPath $file -Raw)
    }
}
Set-Content -LiteralPath $Output -Value $notice.ToString() -Encoding utf8
if ($ModuleReport) {
    Set-Content -LiteralPath $ModuleReport -Value $report -Encoding utf8
}
