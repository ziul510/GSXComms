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

func listenKeyboard(ctx context.Context, apply func(uint32, bool)) {
	// The message pump has to run on the thread that owns the keyboard hook.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	set := user.NewProc("SetWindowsHookExW")
	next := user.NewProc("CallNextHookEx")
	unhook := user.NewProc("UnhookWindowsHookEx")
	peek := user.NewProc("PeekMessageW")
	get := user.NewProc("GetMessageW")
	post := user.NewProc("PostThreadMessageW")
	callback := windows.NewCallback(func(code, wParam, lParam uintptr) uintptr {
		if int32(code) >= 0 && lParam != 0 {
			type lowLevelKey struct {
				Key, Scan, Flags, Time uint32
				Info                   uintptr
			}
			key := (*lowLevelKey)(*(*unsafe.Pointer)(unsafe.Pointer(&lParam))).Key
			if key == captureKey.Load() || selectingKey.Load() {
				switch wParam {
				case 0x0100, 0x0104:
					apply(key, true)
				case 0x0101, 0x0105:
					apply(key, false)
				}
			}
		}
		out, _, _ := next.Call(0, code, wParam, lParam)
		return out
	})
	var module windows.Handle
	if err := windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, nil, &module); err != nil {
		return
	}
	var msg keyboardMsg
	// Create the thread's message queue before cancellation can post WM_QUIT.
	_, _, _ = peek.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 0)
	id := windows.GetCurrentThreadId()
	handle, _, _ := set.Call(13, callback, uintptr(module), 0)
	if handle == 0 {
		return
	}
	defer unhook.Call(handle)
	go func() { <-ctx.Done(); _, _, _ = post.Call(uintptr(id), 0x0012, 0, 0) }()
	for {
		result, _, _ := get.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(result) <= 0 {
			return
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
	// Legacy settings store keys by name. New bindings store virtual key codes.
	var name string
	_ = json.Unmarshal(b.Value, &name)
	name = strings.ToUpper(strings.TrimSpace(name))
	if len(name) == 1 {
		switch c := name[0]; {
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			return uint32(c)
		}
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(name, "F")); strings.HasPrefix(name, "F") && err == nil && n >= 1 && n <= 24 {
		return uint32(0x6f + n)
	}
	return map[string]uint32{"SPACE": 0x20, "ENTER": 0x0d, "TAB": 0x09, "LEFT CTRL": 0xa2, "RIGHT CTRL": 0xa3, "LEFT SHIFT": 0xa0, "RIGHT SHIFT": 0xa1, "LEFT ALT": 0xa4, "RIGHT ALT": 0xa5}[name]
}

func bindingForKey(key uint32) config.Binding {
	name := fmt.Sprintf("VK %d", key)
	if key >= 'A' && key <= 'Z' || key >= '0' && key <= '9' {
		name = string(rune(key))
	}
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
			// A release from the old binding still needs to stop any active recording.
			if !down && key == captureKey.Load() {
				s.record(false)
			}
			if down {
				if key == 0x1b {
					s.binding(config.Binding{})
				} else {
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

func (s *state) rebind(draft config.Values, parent *gui.Window) {
	if s.binding != nil {
		return
	}
	var closeDialog func()
	finished := false
	finish := func(b config.Binding) {
		if finished {
			return
		}
		finished = true
		s.binding = nil
		selectingKey.Store(false)
		if b.Type != "" {
			draft.Put("ptt_bind", b)
		}
		parent.InvalidateLayout()
		if closeDialog != nil {
			closeDialog()
		}
	}
	s.binding = finish
	selectingKey.Store(true)
	closeDialog = s.dialog(gui.WindowCfg{Title: "Bind Push-To-Talk", Width: 380, Height: 185}, func() { finish(config.Binding{}) }, func(w *gui.Window) gui.View {
		return page("bind", []gui.View{text("Bind Push-To-Talk", 18, ink)}, []gui.View{wrapped("Press a keyboard key or controller button. Press Escape to cancel.", 14, ink)}, []gui.View{spacer(), button("cancel-rebind", "Cancel", 0, false, false, func() { finish(config.Binding{}) })})
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
	var last [4]uint16
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for i, prev := range last {
			// A disconnected XInput slot starts with no buttons held.
			var current xinputState
			_, _, _ = proc.Call(uintptr(i), uintptr(unsafe.Pointer(&current)))
			last[i] = current.buttons
			if prev == current.buttons {
				continue
			}
			for button := 0; button < 16; button++ {
				bit := uint16(1) << button
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
					b := config.Get[config.Binding](s.cfg, "ptt_bind")
					var slot int
					if b.Type == "joystick" && b.JoyButton != nil && json.Unmarshal(b.JoyID, &slot) == nil && slot == i && *b.JoyButton == button {
						s.record(isDown)
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
		// Another process can have a window with the same caption.
		if owner == pid {
			return hwnd
		}
	}
}

func (s *state) setTopmost() {
	if hwnd := windowByTitle("GSXComms"); hwnd != 0 {
		// Win32 uses negative pseudo-handles for HWND_NOTOPMOST and HWND_TOPMOST.
		placement := ^uintptr(1)
		if config.Get[bool](s.cfg, "topmost_active") {
			placement = ^uintptr(0)
		}
		_, _, _ = user.NewProc("SetWindowPos").Call(hwnd, placement, 0, 0, 0, 0, 0x0013)
	}
}
