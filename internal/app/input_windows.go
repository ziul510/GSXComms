// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"golang.org/x/sys/windows"
)

var user = windows.NewLazySystemDLL("user32.dll")
var captureKey atomic.Uint32
var selectingKey atomic.Bool

type keyboardMsg struct {
	window         uintptr
	msg            uint32
	wParam, lParam uintptr
	time           uint32
	x, y           int32
	private        uint32
}

// Listen until shutdown or report why Windows cannot deliver keyboard PTT events.
func listenKeyboard(ctx context.Context, apply func(uint32, bool)) error {
	// The message pump has to run on the thread that owns the keyboard hook
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	set := user.NewProc("SetWindowsHookExW")
	next := user.NewProc("CallNextHookEx")
	unhook := user.NewProc("UnhookWindowsHookEx")
	peek := user.NewProc("PeekMessageW")
	get := user.NewProc("GetMessageW")
	post := user.NewProc("PostThreadMessageW")
	// The low level hook receives keyboard transitions even when another window has focus
	callback := windows.NewCallback(func(code, wParam, lParam uintptr) uintptr {
		if int32(code) >= 0 && lParam != 0 {
			type lowLevelKey struct {
				Key, Scan, Flags, Time uint32
				Info                   uintptr
			}
			key := (*lowLevelKey)(*(*unsafe.Pointer)(unsafe.Pointer(&lParam))).Key
			// Forward only the active PTT key unless the rebinding dialog is listening for any key
			if key == captureKey.Load() || selectingKey.Load() {
				switch wParam {
				case 0x0100, 0x0104:
					apply(key, true)
				case 0x0101, 0x0105:
					apply(key, false)
				}
			}
		}
		// Always pass the key onward so PTT doesn't swallow simulator input
		out, _, _ := next.Call(0, code, wParam, lParam)
		return out
	})
	var module windows.Handle
	if err := windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, nil, &module); err != nil {
		return err
	}
	var msg keyboardMsg
	// Create the thread's message queue before cancellation can post WM_QUIT
	_, _, _ = peek.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 0)
	id := windows.GetCurrentThreadId()
	handle, _, err := set.Call(13, callback, uintptr(module), 0)
	if handle == 0 {
		return fmt.Errorf("register keyboard hook: %w", err)
	}
	defer unhook.Call(handle)
	// Wake GetMessage on cancellation so the hook can be removed on its owning thread
	go func() { <-ctx.Done(); _, _, _ = post.Call(uintptr(id), 0x0012, 0, 0) }()
	for {
		result, _, err := get.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		switch int32(result) {
		case -1:
			return fmt.Errorf("read keyboard hook messages: %w", err)
		case 0:
			return nil
		}
	}
}

func bindingKey(b config.Binding) uint32 {
	if b.Type != "key" {
		return 0
	}
	var code uint32
	if json.Unmarshal(b.Value, &code) == nil {
		return code
	}
	// Legacy settings store keys by name New bindings store virtual key codes
	var name string
	_ = json.Unmarshal(b.Value, &name)
	name = strings.ToUpper(strings.TrimSpace(name))
	if len(name) == 1 {
		switch c := name[0]; {
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			return uint32(c)
		}
		if key, _, _ := user.NewProc("VkKeyScanW").Call(uintptr(name[0])); uint16(key) < 0x0100 {
			return uint32(key & 0xff)
		}
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(name, "F")); strings.HasPrefix(name, "F") && err == nil && n >= 1 && n <= 24 {
		return uint32(0x6f + n)
	}
	return map[string]uint32{
		"SPACE": 0x20, "ENTER": 0x0d, "TAB": 0x09, "BACKSPACE": 0x08, "CAPS LOCK": 0x14,
		"LEFT CTRL": 0xa2, "RIGHT CTRL": 0xa3, "CTRL": 0xa2, "LEFT SHIFT": 0xa0, "RIGHT SHIFT": 0xa1, "SHIFT": 0xa0,
		"LEFT ALT": 0xa4, "RIGHT ALT": 0xa5, "ALT": 0xa4, "ALT GR": 0xa5, "LEFT WINDOWS": 0x5b, "RIGHT WINDOWS": 0x5c,
		"PAGE UP": 0x21, "PAGE DOWN": 0x22, "END": 0x23, "HOME": 0x24, "LEFT": 0x25, "UP": 0x26, "RIGHT": 0x27, "DOWN": 0x28,
		"PRINT SCREEN": 0x2c, "INSERT": 0x2d, "DELETE": 0x2e, "NUM LOCK": 0x90, "SCROLL LOCK": 0x91, "PAUSE": 0x13,
	}[name]
}

// Store the numeric Windows key code and keep a separate description for the UI
func bindingForKey(key uint32) config.Binding {
	name := fmt.Sprintf("VK %d", key)
	if key >= 'A' && key <= 'Z' || key >= '0' && key <= '9' {
		name = string(rune(key))
	}
	// VK_F1 through VK_F24 occupy one contiguous range
	if key >= 0x70 && key <= 0x87 {
		name = fmt.Sprintf("F%d", key-0x6f)
	}
	switch key {
	case 0x20:
		name = "Space"
	case 0xa2:
		name = "Left Ctrl"
	case 0xa3:
		name = "Right Ctrl"
	case 0xa4:
		name = "Left Alt"
	case 0xa5:
		name = "Right Alt"
	}
	raw, _ := json.Marshal(key)
	return config.Binding{Type: "key", Value: raw, Description: "Key " + name}
}

func (s *state) keyboardEvent(key uint32, down bool) {
	s.async(func() {
		if s.binding != nil {
			// A release from the old binding still needs to stop any active recording
			if !down && key == captureKey.Load() {
				s.record(false)
			}
			// Bind only on presses and use Escape to cancel
			if down {
				if key == 0x1b {
					s.binding(config.Binding{})
				} else if key != 0x0d {
					// Enter can activate the dialog's focused control, so reserve it for the dialog.
					s.binding(bindingForKey(key))
				}
			}
			return
		}
		if key == captureKey.Load() {
			s.record(down)
		}
	})
}

func (s *state) rebind(parent *gui.Window) {
	// Only one dialog can own the next keyboard or controller input
	if s.binding != nil {
		return
	}
	var closeDialog func()
	var timeout *time.Timer
	var window *gui.Window
	var closed, saving bool
	var message string
	finish := func(b config.Binding) {
		// Escape and input events must wait for the in-flight binding write
		if closed || saving {
			return
		}
		if b.Type == "" {
			closeDialog()
			return
		}
		saving, message = true, "Saving binding…"
		if timeout != nil {
			timeout.Stop()
		}
		selectingKey.Store(false)
		if window != nil {
			window.InvalidateLayout()
		}
		// The live binding changes only after the settings writer confirms the save
		s.saveSetting("ptt_bind", b, func(err error) {
			if closed {
				return
			}
			if err == nil {
				closeDialog()
				return
			}
			saving, message = false, "Could not save binding. Press a key or button to retry."
			selectingKey.Store(true)
			// Give the user a fresh input window after a failed save.
			timeout.Reset(15 * time.Second)
			if window != nil {
				window.InvalidateLayout()
			}
		})
	}
	s.binding = finish
	// Tell the global hook to forward unfamiliar keys until the dialog finishes
	selectingKey.Store(true)
	closeDialog = s.dialog(gui.WindowCfg{Title: "Bind Push-To-Talk", Width: 380, Height: 220}, func() bool { return saving }, func() {
		closed = true
		s.binding = nil
		selectingKey.Store(false)
		if timeout != nil {
			timeout.Stop()
		}
		if !parent.CloseRequested() {
			parent.InvalidateLayout()
		}
	}, func(w *gui.Window) gui.View {
		window = w
		prompt := "Press a keyboard key or controller button. Press Escape to cancel."
		if s.keyboardUnavailable {
			prompt = "Press a controller button or click Cancel. Keyboard capture is unavailable."
		}
		body := []gui.View{wrapped(prompt, 14, ink)}
		if message != "" {
			body = append(body, wrapped(message, 13, muted))
		}
		return page("bind", []gui.View{text("Bind Push-To-Talk", 18, ink)}, body, []gui.View{
			spacer(), button("cancel-rebind", "Cancel", 0, false, saving, closeDialog),
		})
	})
	timeout = time.AfterFunc(15*time.Second, func() {
		s.async(func() {
			if !saving {
				finish(config.Binding{})
			}
		})
	})
}

type xinputState struct {
	packet      uint32
	buttons     uint16
	left, right uint8
	axes        [4]int16
}

func (s *state) pollController(ctx context.Context) {
	proc := windows.NewLazySystemDLL("xinput1_4.dll").NewProc("XInputGetState")
	if proc.Find() != nil {
		return
	}
	t := time.NewTicker(16 * time.Millisecond)
	defer t.Stop()
	// XInput exposes four slots Keep a separate last button mask for each slot
	var last [4]uint16
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for i, prev := range last {
			var current xinputState
			status, _, _ := proc.Call(uintptr(i), uintptr(unsafe.Pointer(&current)))
			if status != 0 {
				// Clear previous buttons but distinguish input loss from a release.
				current.buttons = 0
			}
			last[i] = current.buttons
			if prev == current.buttons {
				continue
			}
			for button := 0; button < 16; button++ {
				bit := uint16(1) << button
				// Emit edges so holding a controller button won't keep triggering capture
				if (prev & bit) == (current.buttons & bit) {
					continue
				}
				isDown := current.buttons&bit != 0
				s.async(func() {
					if s.binding != nil && isDown {
						raw, _ := json.Marshal(i)
						s.binding(config.Binding{Type: "joystick", JoyID: raw, JoyName: fmt.Sprintf("XInput Controller %d", i+1), JoyButton: &button, Description: fmt.Sprintf("XInput %d Button %d", i+1, button+1)})
						return
					}
					// XInput bindings use a numeric slot ID Raw Input bindings can carry a device path
					b := config.Get[config.Binding](s.cfg, "ptt_bind")
					var slot int
					if b.Type == "joystick" && b.JoyButton != nil && strings.HasPrefix(b.JoyName, "XInput Controller ") && json.Unmarshal(b.JoyID, &slot) == nil && slot == i && *b.JoyButton == button {
						if status != 0 {
							s.stopRecording(errPTTInputLost)
						} else {
							s.record(isDown)
						}
					}
				})
			}
		}
	}
}

func windowByTitle(title string) uintptr {
	caption, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return 0
	}
	find := user.NewProc("FindWindowExW")
	getPID := user.NewProc("GetWindowThreadProcessId")
	pid := uint32(os.Getpid())
	var hwnd uintptr
	for {
		hwnd, _, _ = find.Call(0, hwnd, 0, uintptr(unsafe.Pointer(caption)))
		if hwnd == 0 {
			return 0
		}
		var owner uint32
		_, _, _ = getPID.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
		// Another process can have a window with the same caption
		if owner == pid {
			return hwnd
		}
	}
}

func (s *state) setTopmost() {
	if hwnd := windowByTitle("GSXComms"); hwnd != 0 {
		// Use the Win32 constants HWND_NOTOPMOST and HWND_TOPMOST
		placement := ^uintptr(1)
		if config.Get[bool](s.cfg, "topmost_active") {
			placement = ^uintptr(0)
		}
		// Change window stacking without moving, resizing, or activating it
		_, _, _ = user.NewProc("SetWindowPos").Call(hwnd, placement, 0, 0, 0, 0, 0x0013)
	}
}

func focusWindow(title string) {
	if hwnd := windowByTitle(title); hwnd != 0 {
		if minimized, _, _ := user.NewProc("IsIconic").Call(hwnd); minimized != 0 {
			// Restore the existing dialog before bringing it to the foreground.
			_, _, _ = user.NewProc("ShowWindow").Call(hwnd, windows.SW_RESTORE)
		}
		_, _, _ = user.NewProc("SetForegroundWindow").Call(hwnd)
	}
}
