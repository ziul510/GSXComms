// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package mute

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ole = windows.NewLazySystemDLL("ole32.dll")
var free = ole.NewProc("CoTaskMemFree")
var (
	clEnumerator = guid("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	iEnumerator  = guid("{A95664D2-9614-4F35-A746-DE8DB63617E6}")
	iManager     = guid("{77AA99A0-1BD6-484C-8BC7-2C654C9A9B6F}")
	iSession     = guid("{BFB7FF88-7239-4FC9-8FA2-07C950BE9C6D}")
	iVolume      = guid("{87CE5498-68D6-44E5-9215-6DA47EF883D8}")
)

// A malformed built-in COM identifier means the backend cannot run
func guid(s string) windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return g
}

type com struct{ vtable *[32]uintptr }

//go:uintptrescapes
func (p *com) invoke(index int, args ...uintptr) error {
	if p == nil {
		return errors.New("audio COM object unavailable")
	}
	// COM vtable calls take the interface pointer before the method arguments
	values := append([]uintptr{uintptr(unsafe.Pointer(p))}, args...)
	hr, _, _ := syscall.SyscallN(p.vtable[index], values...)
	// COM returns HRESULTs even though the Go syscall returns a machine word
	if int32(hr) < 0 {
		return fmt.Errorf("Core Audio method %d failed (0x%08x)", index, uint32(hr))
	}
	return nil
}
func (p *com) release() {
	// Slot 2 is IUnknown.Release and drops the reference owned by the caller
	if p != nil {
		_ = p.invoke(2)
	}
}
func (p *com) query(id *windows.GUID) (result *com, err error) {
	// Slot 0 is QueryInterface The returned interface owns a separate reference
	err = p.invoke(0, uintptr(unsafe.Pointer(id)), uintptr(unsafe.Pointer(&result)))
	return
}
func (p *com) str(index int) string {
	// String getters return COM-allocated UTF-16 that must be copied and freed
	var value *uint16
	if p.invoke(index, uintptr(unsafe.Pointer(&value))) != nil || value == nil {
		return ""
	}
	defer free.Call(uintptr(unsafe.Pointer(value)))
	return windows.UTF16PtrToString(value)
}

type windowsAudio struct{}

func process(pid uint32) (start uint64, name string, err error) {
	if pid == 0 {
		return 0, "", windows.ERROR_INVALID_PARAMETER
	}
	// Only query the process The journal needs its creation time and executable name
	fd, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, "", err
	}
	defer windows.CloseHandle(fd)
	var a, b, c, d windows.Filetime
	if err := windows.GetProcessTimes(fd, &a, &b, &c, &d); err != nil {
		return 0, "", err
	}
	// Pair the creation FILETIME with the PID to guard against PID reuse
	start = uint64(a.HighDateTime)<<32 | uint64(a.LowDateTime)
	var exit uint32
	// STILL_ACTIVE is 259 An exited process no longer needs audio recovery
	if windows.GetExitCodeProcess(fd, &exit) == nil && exit != 259 {
		return 0, "", windows.ERROR_INVALID_PARAMETER
	}
	// The executable name helps identify Couatl even when the session display name is blank
	var str [4096]uint16
	n := uint32(len(str))
	if windows.QueryFullProcessImageName(fd, 0, &str[0], &n) == nil {
		name = windows.UTF16ToString(str[:n])
	}
	return start, name, nil
}

func (windowsAudio) Alive(id identity) bool {
	start, _, err := process(id.PID)
	if err != nil {
		// An access error alone can't prove the original process exited
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	// Windows can reuse the PID while a restore is still pending
	return start == id.ProcessStarted
}

func (windowsAudio) Visit(fn func(audioSession) error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Initialize COM on the thread that will own and release the audio interfaces
	hr, _, _ := ole.NewProc("CoInitializeEx").Call(0, 0)
	if int32(hr) < 0 {
		return fmt.Errorf("Core Audio initialization failed (0x%08x)", uint32(hr))
	}
	defer ole.NewProc("CoUninitialize").Call()
	var e *com
	hr, _, _ = ole.NewProc("CoCreateInstance").Call(uintptr(unsafe.Pointer(&clEnumerator)), 0, 23, uintptr(unsafe.Pointer(&iEnumerator)), uintptr(unsafe.Pointer(&e)))
	if int32(hr) < 0 {
		return fmt.Errorf("Core Audio enumerator unavailable (0x%08x)", uint32(hr))
	}
	defer e.release()
	var endpoints *com
	// Enumerate active render endpoints so sessions on non-default speakers are included
	if err := e.invoke(3, 0, 1, uintptr(unsafe.Pointer(&endpoints))); err != nil {
		return err
	}
	defer endpoints.release()
	var count uint32
	if err := endpoints.invoke(3, uintptr(unsafe.Pointer(&count))); err != nil {
		return err
	}
	var failures error
	for i := uint32(0); i < count; i++ {
		var endpoint *com
		if endpoints.invoke(4, uintptr(i), uintptr(unsafe.Pointer(&endpoint))) != nil {
			continue
		}
		// One bad endpoint should leave the other devices eligible for recovery
		failures = errors.Join(failures, visitEndpoint(endpoint, fn))
		endpoint.release()
	}
	return failures
}

func visitEndpoint(endpoint *com, fn func(audioSession) error) error {
	// IMMDevice.GetId supplies the endpoint component of the journal identity
	id := endpoint.str(5)
	if id == "" {
		return nil
	}
	var manager, enumerator *com
	// IMMDevice.Activate opens IAudioSessionManager2 for this endpoint
	if err := endpoint.invoke(3, uintptr(unsafe.Pointer(&iManager)), 23, 0, uintptr(unsafe.Pointer(&manager))); err != nil {
		return err
	}
	defer manager.release()
	// Get a snapshot of this endpoint's audio sessions
	if err := manager.invoke(5, uintptr(unsafe.Pointer(&enumerator))); err != nil {
		return err
	}
	defer enumerator.release()
	var count int32
	if err := enumerator.invoke(3, uintptr(unsafe.Pointer(&count))); err != nil {
		return err
	}
	for i := int32(0); i < count; i++ {
		var control *com
		// The session enumerator gives each control its own COM reference
		if enumerator.invoke(4, uintptr(i), uintptr(unsafe.Pointer(&control))) != nil {
			continue
		}
		err := visitSession(control, id, fn)
		control.release()
		if err != nil {
			return err
		}
	}
	return nil
}

func visitSession(control *com, endpoint string, fn func(audioSession) error) error {
	// IAudioSessionControl2 adds the instance identifier and owning PID
	session, err := control.query(&iSession)
	if err != nil {
		return nil
	}
	defer session.release()
	// Use the instance identifier to distinguish sessions from the same process
	instance := session.str(13)
	if instance == "" {
		return nil
	}
	var pid uint32
	if session.invoke(14, uintptr(unsafe.Pointer(&pid))) != nil {
		return nil
	}
	start, name, err := process(pid)
	if err != nil || start == 0 {
		return nil
	}
	// ISimpleAudioVolume gives per-session mute access without changing endpoint volume
	volume, err := control.query(&iVolume)
	if err != nil {
		return nil
	}
	defer volume.release()
	var current uint32
	// Read GetMute so recovery can preserve the original state
	if volume.invoke(6, uintptr(unsafe.Pointer(&current))) != nil {
		return nil
	}
	// Any of the process name, display name, or session ID can identify GSX
	label := filepath.Base(name) + " " + control.str(4) + " " + session.str(12)
	return fn(audioSession{identity{endpoint, instance, pid, start}, current != 0, label, func(muted bool) error {
		var flag uintptr
		if muted {
			flag = 1
		}
		// SetMute takes a Windows BOOL and an optional event context pointer
		return volume.invoke(5, flag, 0)
	}})
}

func Run(dir string, hold func() error) error {
	path := filepath.Join(dir, "gsx-mute.json")
	// Portable copies still mute the same audio sessions, so share one mutex.
	const name = `Local\GSXCommsGo.Mute`
	mutex, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(name))
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	defer windows.CloseHandle(mutex)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Windows mutex ownership stays with the thread that acquires it
	rc, err := windows.WaitForSingleObject(mutex, 5000)
	if err != nil {
		return err
	}
	// An abandoned mutex still gives this thread ownership so it can recover the journal
	if rc != 0 && rc != 0x80 {
		return errors.New("another GSX mute request is still active")
	}
	defer windows.ReleaseMutex(mutex)
	api := windowsAudio{}
	// Recovery-only mode replays the journal without claiming a new mute
	if hold == nil {
		return restore(path, api)
	}
	if err := acquire(path, api); err != nil {
		return errors.Join(err, restore(path, api))
	}
	// Keep the mutex through playback and restore even if waiting for playback failed
	return errors.Join(hold(), restore(path, api))
}
