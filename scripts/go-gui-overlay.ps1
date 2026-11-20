# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
$module = go list -m -f '{{.Dir}}' github.com/go-gui-org/go-gui
$version = go list -m -f '{{.Version}}' github.com/go-gui-org/go-gui
# Stop if the selected module differs from the source these patches target
if ($version -ne 'v0.96.0' -or !$module) { throw 'The Windows software adapter targets go-gui v0.96.0' }
# Patch a local copy so other builds can keep using the upstream module cache
$destination = Join-Path $PWD 'build/go-gui-win32'
$copy = Join-Path $destination 'module'
New-Item -ItemType Directory -Force $copy | Out-Null
if (!(Test-Path -LiteralPath (Join-Path $copy 'go.mod'))) {
    Copy-Item -Path (Join-Path $module '*') -Destination $copy -Recurse -Force
}
# Module cache copies may be read-only Make only the local output writable
function Publish-Patch([string]$relative,[string]$source) {
    $name = Join-Path $copy $relative
    if (Test-Path -LiteralPath $name) { [IO.File]::SetAttributes($name, [IO.FileAttributes]::Normal) }
    [IO.File]::WriteAllText($name,$source)
}
# Start from the upstream source each time so repeated runs can't apply patches twice
function Source([string]$relative) {
    return [IO.File]::ReadAllText((Join-Path $module $relative)).Replace(([string][char]13+[char]10),[string][char]10)
}
function Change([string]$source,[string]$before,[string]$after) {
    # A source mismatch means the patch needs checking against upstream before building
    if (!$source.Contains($before)) { throw ('Unexpected upstream source around ' + $before) }
    return $source.Replace($before,$after)
}

# Add alternate presentation and resize hooks while leaving the normal GL path available
$file = 'gui/backend/gl/backend.go'
$src = Source $file
$src = Change $src 'type Backend struct {' ("type Backend struct {"+[char]10+'softwarePresent func(*gui.Window)'+[char]10+'softwareRelease func()')
$src = Change $src 'func (b *Backend) renderFrame(w *gui.Window) {' @'
func (b *Backend) renderFrame(w *gui.Window) {
    if b.softwarePresent != nil { b.softwarePresent(w); return }
'@
$src = Change $src 'func (b *Backend) handleResize() {' @'
func (b *Backend) handleResize() {
    if b.softwarePresent != nil {
        b.physW, b.physH = b.plat.drawableSize()
        b.dpiScale = b.plat.dpiScale()
        return
    }
'@
Publish-Patch $file $src

$file = 'gui/backend/gl/platform_win32.go'
$src = Source $file
$src = Change $src ('gogl "github.com/go-gui-org/go-gui/gui/backend/internal/glbind"') ''
# Keep the native window setup and replace only the renderer initialization
$start = $src.IndexOf('hglrc, err := createContextFunc(hdc)')
$end = $src.IndexOf('loadCursors(&b.plat)', $start)
if ($start -lt 0 -or $end -lt 0) { throw 'Unexpected Win32 renderer initialization' }
$src = $src.Substring(0,$start) + @'
if err := b.win32Renderer(w); err != nil {
        b.plat.destroy()
        return nil, err
    }


'@ + $src.Substring($end)
# Software presentation needs its own release path before destroying the window
$src = Change $src 'func (b *Backend) Destroy() {' @'
func (b *Backend) Destroy() {
    if b.softwareRelease != nil {
        b.softwareRelease()
        b.plat.destroy()
        return
    }
'@
Publish-Patch $file $src

# Repaint the cached bitmap if Windows asks for a paint between application frames
$file='gui/backend/gl/events_win32.go'
$src=Source $file
$src=Change $src 'case wmPaint:' @'
case wmPaint:
        if b.softwarePresent != nil { b.softwarePresent(nil) }
'@
Publish-Patch $file $src

# The software display needs text measurement without running headless window initialization
$file='gui/backend/soft/render.go'
$src=Source $file
$src=Change $src 'tm, err := prepare(w, scale)' 'tm, err := prepare(w, scale, true)'
$src=Change $src 'func prepare(w *gui.Window, scale float32) (*textMeasurer, error) {' 'func prepare(w *gui.Window, scale float32, initialize bool) (*textMeasurer, error) {'
$src=Change $src '} else if w.Config.OnInit != nil && len(w.Renderers()) == 0 {' '} else if initialize && w.Config.OnInit != nil && len(w.Renderers()) == 0 {'
# Reuse the paint stage without rerunning UI events or changing headless state
$src=Change $src 'logicalW, logicalH := w.WindowSize()' @'
    w.BackingScale = scale
    w.SetHeadlessRender(true)
    w.TestRender(nil)
    return drawDisplay(w, tm, scale)
}

func PrepareDisplay(w *gui.Window, scale float32) error {
    _, err := prepare(w, scale, false)
    return err
}

func RenderDisplay(w *gui.Window, scale float32) (*image.RGBA, error) {
    tm, err := prepare(w, scale, false)
    if err != nil { return nil, err }
    w.BackingScale = scale
    return drawDisplay(w, tm, scale)
}

func drawDisplay(w *gui.Window, tm *textMeasurer, scale float32) (*image.RGBA, error) {
    logicalW, logicalH := w.WindowSize()
'@
# Remove the headless update from the shared draw path now that the caller owns it
$repeated = (
    ([string][char]9 + 'w.BackingScale = scale'),
    ([string][char]9 + 'w.SetHeadlessRender(true)'),
    ([string][char]9 + 'w.TestRender(nil)'),
    ([string][char]9 + 'cmds := w.Renderers()')
) -join [char]10
$src=Change $src $repeated ([string][char]9+'cmds := w.Renderers()')
Publish-Patch $file $src

# Install the adapter next to the patched GL backend so it can use internal renderer methods
Publish-Patch 'gui/backend/gl/gsxcomms_software_windows.go' ([IO.File]::ReadAllText((Join-Path $PSScriptRoot 'win32-software.go.in')))
# The alternate modfile keeps the local replacement out of go.mod
$modfile = Join-Path $destination 'gsxcomms.mod'
Copy-Item -LiteralPath 'go.mod' -Destination $modfile -Force
Copy-Item -LiteralPath 'go.sum' -Destination (Join-Path $destination 'gsxcomms.sum') -Force
go mod edit -modfile $modfile "-replace=github.com/go-gui-org/go-gui=$copy"
if ($LASTEXITCODE -ne 0) { throw 'Could not point the Windows build at the patched module' }
return $modfile
