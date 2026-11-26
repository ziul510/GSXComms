// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/go-gui-org/go-gui/gui/backend"
	"golang.org/x/sys/windows"
)

// Check the native caption before the dialog's OnInit or any focus change
func TestLiveInitialDarkTitleBars(t *testing.T) {
	if os.Getenv("GSXCOMMS_TEST_LIVE_WINDOWS_UI") != "1" {
		t.Skip("enable with GSXCOMMS_TEST_LIVE_WINDOWS_UI=1 on an interactive Windows desktop")
	}
	const mainTitle, dialogTitle = "GSXComms", "GSXComms title bar test"
	const wmShowWindow = 0x0018
	getDark := windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmGetWindowAttribute")
	seen := make(map[string]bool)
	callback := windows.NewCallback(func(code int32, sentByThread uintptr, msg *windowMessageResult) uintptr {
		if code == 0 && msg.message == wmShowWindow && msg.wParam != 0 {
			for _, title := range []string{mainTitle, dialogTitle} {
				if !seen[title] && msg.hwnd == windowByTitle(title) {
					seen[title] = true
					var dark int32
					result, _, _ := getDark.Call(msg.hwnd, dwmImmersiveDarkMode, uintptr(unsafe.Pointer(&dark)), unsafe.Sizeof(dark))
					// Older Windows 10 builds expose the flag under the previous number
					if result != 0 {
						result, _, _ = getDark.Call(msg.hwnd, dwmImmersiveDarkModeOld, uintptr(unsafe.Pointer(&dark)), unsafe.Sizeof(dark))
					}
					if result != 0 || dark != 1 {
						t.Errorf("%s first show: immersive dark mode = %d, HRESULT %#x", title, dark, result)
					}
				}
			}
		}
		result, _, _ := callNextHookEx.Call(0, uintptr(code), sentByThread, uintptr(unsafe.Pointer(msg)))
		return result
	})
	// The observer and the backend must run on the same Windows thread
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hook, _, err := user.NewProc("SetWindowsHookExW").Call(whCallWndProcRet, callback, 0, uintptr(windows.GetCurrentThreadId()))
	if hook == 0 {
		t.Fatalf("install first-show observer: %v", err)
	}
	defer user.NewProc("UnhookWindowsHookEx").Call(hook)
	s := &state{host: gui.NewApp()}
	// Keep the event loop alive until both native windows are destroyed
	s.host.ExitMode = gui.ExitOnTrayRemoved
	var stopDialogHook func()
	main := gui.NewWindow(gui.WindowCfg{Title: mainTitle, Width: 280, Height: 220, OnInit: func(w *gui.Window) {
		stopDialogHook = installDialogWindowHook()
		s.dialog(gui.WindowCfg{
			Title: dialogTitle, Width: 280, Height: 220,
			OnInit: func(dialog *gui.Window) {
				dialog.Close()
				w.Close()
			},
		}, func() bool { return false }, func() {}, func(*gui.Window) gui.View {
			return gui.Label(dialogTitle, gui.TextStyle{})
		})
	}})
	main.TitlebarDark(true)
	// Close the test windows even if the dialog never opens
	timeout := time.AfterFunc(3*time.Second, func() {
		s.host.Broadcast(func(w *gui.Window) { w.Close() })
	})
	defer timeout.Stop()
	backend.RunApp(s.host, main)
	if stopDialogHook != nil {
		stopDialogHook()
	}
	for _, title := range []string{mainTitle, dialogTitle} {
		if !seen[title] {
			t.Errorf("%s first show was not observed", title)
		}
	}
}
