// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"image"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	whCallWndProcRet = 12
	wmCreate         = 0x0001
	wmSize           = 0x0005
	gclAtom          = ^uintptr(31)
	gwlExStyle       = ^uintptr(19)
	wsExTopmost      = 0x0008
	swpNoZOrder      = 0x0004
	swpNoActivate    = 0x0010
	dpiPerMonitor    = 2
	defaultDPI       = 96
	// Windows 10 versions 1809 and 1909 use the older DWM attribute number
	dwmImmersiveDarkMode    = 20
	dwmImmersiveDarkModeOld = 19
)

// The app's window thread owns the creation hook and its target
var dialogPlacement struct {
	main, class uintptr
}

// Use CWPRETSTRUCT to identify a window that just handled WM_CREATE
type windowMessageResult struct {
	result, lParam, wParam uintptr
	message                uint32
	hwnd                   uintptr
}

// Keep monitor and window bounds in the same Win32 screen coordinates
type screenRect struct {
	left, top, right, bottom int32
}

// Convert a native rectangle for geometry calculations
func (r screenRect) rectangle() image.Rectangle {
	return image.Rect(int(r.left), int(r.top), int(r.right), int(r.bottom))
}

// MONITORINFO supplies the work area inside each monitor's taskbar bounds
type monitorInfo struct {
	size          uint32
	monitor, work screenRect
	flags         uint32
}

// Monitor scale lets placement preserve the dialog's logical size across displays
type workArea struct {
	bounds image.Rectangle
	scale  int
}

var (
	getClassLong        = user.NewProc("GetClassLongW")
	getWindowLong       = user.NewProc("GetWindowLongW")
	getWindowRect       = user.NewProc("GetWindowRect")
	getMonitorInfo      = user.NewProc("GetMonitorInfoW")
	enumDisplayMonitors = user.NewProc("EnumDisplayMonitors")
	getSystemDPI        = user.NewProc("GetDpiForSystem")
	dpiAwarenessContext = user.NewProc("GetWindowDpiAwarenessContext")
	dpiAwareness        = user.NewProc("GetAwarenessFromDpiAwarenessContext")
	getMonitorScale     = windows.NewLazySystemDLL("shcore.dll").NewProc("GetScaleFactorForMonitor")
	setWindowPos        = user.NewProc("SetWindowPos")
	isWindowVisible     = user.NewProc("IsWindowVisible")
	isIconic            = user.NewProc("IsIconic")
	callNextHookEx      = user.NewProc("CallNextHookEx")
	dwmSetAttribute     = windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
	dialogCreateHook    = windows.NewCallback(afterDialogCreate)
	monitorCallback     = windows.NewCallback(collectWorkArea)
)

// Install on the GUI thread before go-gui creates any secondary windows
func installDialogWindowHook() func() {
	main := windowByTitle("GSXComms")
	class, _, _ := getClassLong.Call(main, gclAtom)
	// A missing native handle leaves Windows' default placement available
	if class == 0 {
		return nil
	}
	hook, _, _ := user.NewProc("SetWindowsHookExW").Call(whCallWndProcRet, dialogCreateHook, 0, uintptr(windows.GetCurrentThreadId()))
	// Only publish the target if Windows accepted the hook
	if hook == 0 {
		return nil
	}
	dialogPlacement.main, dialogPlacement.class = main, class
	return func() {
		user.NewProc("UnhookWindowsHookEx").Call(hook)
		dialogPlacement.main, dialogPlacement.class = 0, 0
	}
}

// Set the title bar and position before go-gui sizes or shows the dialog
func afterDialogCreate(code int32, sentByThread uintptr, msg *windowMessageResult) uintptr {
	// Other messages and helper windows from different classes use default placement
	if code == 0 && msg.message == wmCreate && msg.result == 0 && msg.hwnd != dialogPlacement.main {
		class, _, _ := getClassLong.Call(msg.hwnd, gclAtom)
		if class != 0 && class == dialogPlacement.class {
			setInitialDialogTitlebarDark(msg.hwnd)
			placeDialogWindow(msg.hwnd, dialogPlacement.main)
		}
	}
	result, _, _ := callNextHookEx.Call(0, uintptr(code), sentByThread, uintptr(unsafe.Pointer(msg)))
	return result
}

// Set the caption while the dialog is hidden, before go-gui calls OnInit
func setInitialDialogTitlebarDark(hwnd uintptr) {
	// Leave the caption to Windows if DWM is unavailable
	if dwmSetAttribute.Find() != nil {
		return
	}
	dark := int32(1)
	if hr, _, _ := dwmSetAttribute.Call(hwnd, dwmImmersiveDarkMode, uintptr(unsafe.Pointer(&dark)), unsafe.Sizeof(dark)); hr != 0 {
		// Older Windows 10 builds may reject the current attribute number
		dwmSetAttribute.Call(hwnd, dwmImmersiveDarkModeOld, uintptr(unsafe.Pointer(&dark)), unsafe.Sizeof(dark))
	}
}

// Collect current work areas so placement can use any connected display
func collectWorkArea(monitor, _ uintptr, _ *screenRect, areas *[]workArea) uintptr {
	info := monitorInfo{size: uint32(unsafe.Sizeof(monitorInfo{}))}
	// Ignore displays that fail to report usable work bounds
	if ok, _, _ := getMonitorInfo.Call(monitor, uintptr(unsafe.Pointer(&info))); ok != 0 {
		*areas = append(*areas, workArea{info.work.rectangle(), monitorScale(monitor)})
	}
	return 1
}

// Use the monitor's effective scale percentage for native window dimensions
func monitorScale(monitor uintptr) int {
	var scale int32
	// Unknown scale leaves the original pixel dimensions available
	if result, _, _ := getMonitorScale.Call(monitor, uintptr(unsafe.Pointer(&scale))); result != 0 {
		return 0
	}
	return int(scale)
}

// Apply position and size without changing the normal Windows stacking order
func placeDialogWindow(hwnd, mainHWND uintptr) {
	var nativeDialog, nativeMain screenRect
	// Keep the native position if Windows cannot supply the dialog bounds
	if ok, _, _ := getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&nativeDialog))); ok == 0 {
		return
	}
	mainStyle, _, _ := getWindowLong.Call(mainHWND, gwlExStyle)
	mainVisible, _, _ := isWindowVisible.Call(mainHWND)
	mainMinimized, _, _ := isIconic.Call(mainHWND)
	// Only a visible topmost main window can cover a newly activated dialog
	if mainStyle&wsExTopmost != 0 && mainVisible != 0 && mainMinimized == 0 {
		getWindowRect.Call(mainHWND, uintptr(unsafe.Pointer(&nativeMain)))
	}
	var areas []workArea
	enumDisplayMonitors.Call(0, 0, monitorCallback, uintptr(unsafe.Pointer(&areas)))
	context, _, _ := dpiAwarenessContext.Call(hwnd)
	awareness, _, _ := dpiAwareness.Call(context)
	sourceScale := 0
	// Go-gui creates the native frame at system DPI unless its scale is pinned
	if awareness == dpiPerMonitor && os.Getenv("GOGUI_DEVICE_SCALE") == "" {
		dpi, _, _ := getSystemDPI.Call()
		sourceScale = (int(dpi)*100 + defaultDPI/2) / defaultDPI
	}
	intended := nativeDialog.rectangle()
	placed := dialogPosition(intended, nativeMain.rectangle(), sourceScale, areas)
	// Avoid extra native move and resize messages for a position that already fits
	if placed != intended {
		moved, _, _ := setWindowPos.Call(hwnd, 0, uintptr(placed.Min.X), uintptr(placed.Min.Y),
			uintptr(placed.Dx()), uintptr(placed.Dy()), swpNoZOrder|swpNoActivate)
		if moved != 0 {
			// go-gui may cache the placed size before it updates the logical size
			user.NewProc("PostMessageW").Call(hwnd, wmSize, 0, 0)
		}
	}
}

// Keep the entire dialog in a work area and prefer space clear of the main window
func dialogPosition(dialog, main image.Rectangle, sourceScale int, areas []workArea) image.Rectangle {
	best := dialog
	bestVisible, bestDistance := -1.0, 0
	// A failed native size query should never send an empty resize
	if dialog.Empty() {
		return dialog
	}
	// Check each display so a large main window can send the dialog to another one
	for _, display := range areas {
		area := display.bounds
		// Windows can enumerate virtual displays without a drawable work area
		if area.Empty() {
			continue
		}
		// Keep a usable default placement exactly where Windows intended
		if dialog.In(area) && !dialog.Overlaps(main) {
			return dialog
		}
		desired := dialog.Size()
		// Scale the entire native frame before testing a differently scaled display
		if sourceScale > 0 && display.scale > 0 {
			desired.X = max(1, (desired.X*display.scale+sourceScale/2)/sourceScale)
			desired.Y = max(1, (desired.Y*display.scale+sourceScale/2)/sourceScale)
		}
		// Keep a usable window even when its default size exceeds the entire display
		size := image.Pt(min(desired.X, area.Dx()), min(desired.Y, area.Dy()))
		limit := area.Max.Sub(size)
		// The intended position and the edges of the two rectangles bound every fit
		xs := []int{dialog.Min.X, area.Min.X, limit.X, main.Min.X - size.X, main.Max.X}
		ys := []int{dialog.Min.Y, area.Min.Y, limit.Y, main.Min.Y - size.Y, main.Max.Y}
		for _, x := range xs {
			for _, y := range ys {
				origin := image.Pt(min(max(x, area.Min.X), limit.X), min(max(y, area.Min.Y), limit.Y))
				candidate := image.Rectangle{Min: origin, Max: origin.Add(size)}
				covered := candidate.Intersect(main)
				// Compare the visible fraction so higher DPI alone does not win
				visible := float64(size.X*size.Y-covered.Dx()*covered.Dy()) / float64(desired.X*desired.Y)
				// Prefer a fully exposed dialog even if it needs to fit a smaller display
				if covered.Empty() {
					visible++
				}
				offset := origin.Sub(dialog.Min)
				distance := offset.X*offset.X + offset.Y*offset.Y
				// For equivalent visibility, stay closest to Windows' intended origin
				if visible > bestVisible || visible == bestVisible && distance < bestDistance {
					best, bestVisible, bestDistance = candidate, visible, distance
				}
			}
		}
	}
	return best
}
