// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unsafe"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"golang.org/x/sys/windows"
)

type rawDevice struct {
	page, usage uint16
	flags       uint32
	target      uintptr
}

type rawDeviceEntry struct {
	handle uintptr
	kind   uint32
}

func (s *state) refreshHIDPaths() {
	list := user.NewProc("GetRawInputDeviceList")
	size := unsafe.Sizeof(rawDeviceEntry{})
	var count uint32
	result, _, _ := list.Call(0, uintptr(unsafe.Pointer(&count)), size)
	if uint32(result) == ^uint32(0) || count > 4096 {
		return
	}
	paths := make(map[string][]string)
	if count > 0 {
		devices := make([]rawDeviceEntry, int(count))
		result, _, _ = list.Call(uintptr(unsafe.Pointer(&devices[0])), uintptr(unsafe.Pointer(&count)), size)
		if uint32(result) == ^uint32(0) || result > uintptr(len(devices)) {
			return
		}
		for _, entry := range devices[:result] {
			if entry.kind == 2 {
				if hid := readHIDDevice(entry.handle); hid != nil {
					name := strings.ToLower(strings.TrimSpace(hid.name))
					paths[name] = append(paths[name], hid.path)
				}
			}
		}
	}
	s.async(func() {
		s.hidPaths = paths
		s.host.Broadcast(func(w *gui.Window) { w.InvalidateLayout() })
	})
}

func (s *state) legacyHIDPath(b config.Binding) string {
	paths := s.hidPaths[strings.ToLower(strings.TrimSpace(b.JoyName))]
	if len(paths) == 1 {
		return paths[0]
	}
	return ""
}

var hidModelPattern = regexp.MustCompile("vid_[0-9a-f]{4}&pid_[0-9a-f]{4}")

func (s *state) boundHIDPath(b config.Binding) string {
	var path string
	if json.Unmarshal(b.JoyID, &path) != nil || !strings.HasPrefix(path, "\\\\?\\") {
		return ""
	}
	model := hidModelPattern.FindString(strings.ToLower(path))
	var fallback string
	count := 0
	for _, paths := range s.hidPaths {
		for _, candidate := range paths {
			if strings.EqualFold(candidate, path) {
				return candidate
			}
			if model != "" && hidModelPattern.FindString(strings.ToLower(candidate)) == model {
				fallback = candidate
				count++
			}
		}
	}
	if count == 1 {
		return fallback
	}
	return ""
}

// Report whether the saved binding identifies a usable input
func (s *state) bindingDescription(b config.Binding) (string, bool) {
	switch b.Type {
	case "key":
		if bindingKey(b) != 0 {
			return b.Description, true
		}
	case "joystick":
		var slot int
		if b.JoyButton != nil && *b.JoyButton >= 0 {
			if s.boundHIDPath(b) != "" ||
				(strings.HasPrefix(b.JoyName, "XInput Controller ") && json.Unmarshal(b.JoyID, &slot) == nil && slot >= 0 && slot < 4 && *b.JoyButton < 16) {
				return b.Description, true
			}
			if s.legacyHIDPath(b) != "" {
				return "Unverified name match · Rebind required", false
			}
		}
	}
	return "Rebind required", false
}

type rawHeader struct {
	kind, size   uint32
	device, code uintptr
}

type hidDevice struct {
	path, name string
	preparsed  []byte
	buttons    []uint16
}

var (
	hid          = windows.NewLazySystemDLL("hid.dll")
	getRawDevice = user.NewProc("GetRawInputDeviceInfoW")
	getUsages    = hid.NewProc("HidP_GetUsages")
)

func readHIDDevice(handle uintptr) *hidDevice {
	// Query the path separately from the preparsed data used to decode HID reports
	const deviceName = 0x20000007
	const preparse = 0x20000005
	var size uint32
	result, _, _ := getRawDevice.Call(handle, deviceName, 0, uintptr(unsafe.Pointer(&size)))
	if uint32(result) == 0xffffffff || size == 0 || size > 4096 {
		return nil
	}
	// RIDI_DEVICENAME returns a character count RIDI_PREPARSEDDATA returns bytes
	buf := make([]uint16, size)
	result, _, _ = getRawDevice.Call(handle, deviceName, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if uint32(result) == 0xffffffff {
		return nil
	}
	path := windows.UTF16ToString(buf)
	// Skip IG_ devices here to avoid duplicate button events from XInput
	if strings.Contains(strings.ToLower(path), "ig_") {
		return nil
	}
	// Use the path as a display name only until the device supplies a product string
	product := path
	if fd, err := windows.CreateFile(&buf[0], 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0); err == nil {
		var name [128]uint16
		r, _, _ := hid.NewProc("HidD_GetProductString").Call(uintptr(fd), uintptr(unsafe.Pointer(&name[0])), unsafe.Sizeof(name))
		_ = windows.CloseHandle(fd)
		if r != 0 && name[0] != 0 {
			product = windows.UTF16ToString(name[:])
		}
	}
	size = 0
	result, _, _ = getRawDevice.Call(handle, preparse, 0, uintptr(unsafe.Pointer(&size)))
	if uint32(result) == 0xffffffff || size == 0 || size > 1<<16 {
		return nil
	}
	data := make([]byte, size)
	result, _, _ = getRawDevice.Call(handle, preparse, uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&size)))
	if uint32(result) == 0xffffffff {
		return nil
	}
	return &hidDevice{path: path, name: product, preparsed: data}
}

func (s *state) listenHID(ctx context.Context) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var module windows.Handle
	if err := windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, nil, &module); err != nil {
		return
	}
	// A message-only window receives controller input without adding a visible window
	class := windows.StringToUTF16Ptr("STATIC")
	hwnd, _, _ := user.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(class)), 0, 0, 0, 0, 0, 0, ^uintptr(2), 0, uintptr(module), 0)
	if hwnd == 0 {
		return
	}
	defer user.NewProc("DestroyWindow").Call(hwnd)
	// INPUTSINK gets controls in the background DEVNOTIFY reports unplug events
	devices := [3]rawDevice{{1, 4, 0x2100, hwnd}, {1, 5, 0x2100, hwnd}, {1, 8, 0x2100, hwnd}}
	ok, _, _ := user.NewProc("RegisterRawInputDevices").Call(uintptr(unsafe.Pointer(&devices[0])), uintptr(len(devices)), unsafe.Sizeof(devices[0]))
	if ok == 0 {
		return
	}
	get := user.NewProc("GetMessageW")
	dispatch := user.NewProc("DispatchMessageW")
	post := user.NewProc("PostThreadMessageW")
	raw := user.NewProc("GetRawInputData")
	id := windows.GetCurrentThreadId()
	go func() { <-ctx.Done(); _, _, _ = post.Call(uintptr(id), 0x12, 0, 0) }()
	known := make(map[uintptr]*hidDevice)
	s.refreshHIDPaths()
	for {
		var msg keyboardMsg
		rc, _, _ := get.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(rc) <= 0 {
			return
		}
		switch msg.msg {
		// On device removal, release held buttons and discard the cached preparsed data
		case 0x00fe:
			if msg.wParam == 2 {
				if d := known[msg.lParam]; d != nil {
					path := d.path
					s.async(func() {
						if strings.EqualFold(s.heldHIDPath, path) {
							s.record(false)
						}
					})
				}
				delete(known, msg.lParam)
			}
			s.refreshHIDPaths()
		// WM_INPUT carries a variable-size packet Ask Windows how much data to allocate
		case 0x00ff:
			var size uint32
			const input = 0x10000003
			head := unsafe.Sizeof(rawHeader{})
			_, _, _ = raw.Call(msg.lParam, input, 0, uintptr(unsafe.Pointer(&size)), head)
			if size < uint32(head)+8 || size > 1<<16 {
				break
			}
			data := make([]byte, size)
			got, _, _ := raw.Call(msg.lParam, input, uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&size)), head)
			if uint32(got) != size {
				break
			}
			h := (*rawHeader)(unsafe.Pointer(&data[0]))
			if h.kind != 2 {
				break
			}
			d, seen := known[h.device]
			// Cache rejected devices too so every report doesn't reopen the same device
			if !seen {
				d = readHIDDevice(h.device)
				known[h.device] = d
			}
			if d == nil {
				break
			}
			// RAWHID stores report size and count immediately after the raw input header
			length := binary.LittleEndian.Uint32(data[head:])
			count := binary.LittleEndian.Uint32(data[head+4:])
			// Check the full byte count before slicing any of the reports
			if length == 0 || uint64(length)*uint64(count) > uint64(len(data)-int(head)-8) {
				break
			}
			// One WM_INPUT message can batch several reports
			for i := uint32(0); i < count; i++ {
				offset := head + 8 + uintptr(i*length)
				s.hidReport(d, data[offset:offset+uintptr(length)])
			}
		}
		_, _, _ = dispatch.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func (s *state) hidReport(d *hidDevice, report []byte) {
	var usages [256]uint16
	size := uint32(len(usages))
	// Use the device's preparsed data to decode input usages on the Button page
	code, _, _ := getUsages.Call(0, 9, 0, uintptr(unsafe.Pointer(&usages[0])), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&d.preparsed[0])), uintptr(unsafe.Pointer(&report[0])), uintptr(len(report)))
	if uint32(code) != 0x00110000 || size > uint32(len(usages)) {
		return
	}
	next := usages[:size]
	// HID reports list held buttons, so compare with the prior report for releases
	for _, value := range d.buttons {
		// HID button usages are one-based The settings store zero-based indices
		if value > 0 && !slices.Contains(next, value) {
			s.hidButton(d, int(value)-1, false)
		}
	}
	for _, value := range next {
		if value > 0 && !slices.Contains(d.buttons, value) {
			s.hidButton(d, int(value)-1, true)
		}
	}
	d.buttons = append(d.buttons[:0], next...)
}

func (s *state) hidButton(device *hidDevice, index int, down bool) {
	path, name := device.path, device.name
	s.async(func() {
		if !down {
			// A hotplug can change name resolution while the original button is held.
			if s.heldHIDPath != "" && strings.EqualFold(s.heldHIDPath, path) && s.heldHIDButton == index {
				s.record(false)
			}
			return
		}
		if s.binding != nil {
			id, _ := json.Marshal(path)
			s.binding(config.Binding{Type: "joystick", JoyID: id, JoyName: name, JoyButton: &index, Description: fmt.Sprintf("%s Button %d", name, index+1)})
			return
		}
		b := config.Get[config.Binding](s.cfg, "ptt_bind")
		if s.pttHeld || b.Type != "joystick" || b.JoyButton == nil || *b.JoyButton != index {
			return
		}
		if id := s.boundHIDPath(b); id != "" && strings.EqualFold(id, path) {
			s.heldHIDPath, s.heldHIDButton = path, index
			s.record(true)
		}
	})
}
