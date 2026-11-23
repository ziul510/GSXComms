// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package mute

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ole = windows.NewLazySystemDLL("ole32.dll")
var free = ole.NewProc("CoTaskMemFree")
var (
	clEnumerator = guid("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	iEnumerator  = guid("{A95664D2-9614-4F35-A746-DE8DB63617E6}")
	iManager     = guid("{77AA99A0-1BD6-484F-8BC7-2C654C9A9B6F}") // IAudioSessionManager2 in audiopolicy.h
	iSession     = guid("{BFB7FF88-7239-4FC9-8FA2-07C950BE9C6D}")
	iVolume      = guid("{87CE5498-68D6-44E5-9215-6DA47EF883D8}")
)

// An invalid COM identifier is a source error
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
	// Slot 2 releases the caller's interface reference
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
	// Copy the UTF16 string before freeing the COM buffer
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
	// Keep this goroutine on the same thread until COM cleanup has run.
	defer runtime.UnlockOSThread()
	// Initialize COM on the thread that will own and release the audio interfaces
	hr, _, _ := ole.NewProc("CoInitializeEx").Call(0, 0)
	if int32(hr) < 0 {
		return fmt.Errorf("Core Audio initialization failed (0x%08x)", uint32(hr))
	}
	// Register COM cleanup before acquiring interfaces so their releases run first.
	defer ole.NewProc("CoUninitialize").Call()
	var e *com
	hr, _, _ = ole.NewProc("CoCreateInstance").Call(uintptr(unsafe.Pointer(&clEnumerator)), 0, 23, uintptr(unsafe.Pointer(&iEnumerator)), uintptr(unsafe.Pointer(&e)))
	if int32(hr) < 0 {
		return fmt.Errorf("Core Audio enumerator unavailable (0x%08x)", uint32(hr))
	}
	defer e.release()
	var endpoints *com
	// Enumerate active render endpoints so sessions on other speakers are included
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
		if err := endpoints.invoke(4, uintptr(i), uintptr(unsafe.Pointer(&endpoint))); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		// One bad endpoint should leave the other devices eligible for recovery
		failures = errors.Join(failures, visitEndpoint(endpoint, fn))
		endpoint.release()
	}
	return failures
}

func visitEndpoint(endpoint *com, fn func(audioSession) error) error {
	// Use the device ID as part of the journal identity
	id := endpoint.str(5)
	if id == "" {
		return nil
	}
	var manager, enumerator *com
	// Open the session manager for this endpoint
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
	var failures error
	for i := int32(0); i < count; i++ {
		var control *com
		// The session enumerator gives each control its own COM reference
		if err := enumerator.invoke(4, uintptr(i), uintptr(unsafe.Pointer(&control))); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		failures = errors.Join(failures, visitSession(control, id, fn))
		control.release()
	}
	return failures
}

func visitSession(control *com, endpoint string, fn func(audioSession) error) error {
	// IAudioSessionControl2 adds the instance identifier and owning PID
	session, err := control.query(&iSession)
	if err != nil {
		return nil
	}
	defer session.release()
	var pid uint32
	if session.invoke(14, uintptr(unsafe.Pointer(&pid))) != nil {
		return nil
	}
	start, name, err := process(pid)
	// Use metadata as a fallback when the process name is unavailable
	label := filepath.Base(name) + " " + control.str(4) + " " + session.str(12)
	target := match(label)
	if err != nil || start == 0 {
		if target {
			return errors.New("matching audio session has no recoverable identity")
		}
		return nil
	}
	// The instance identifier distinguishes sessions from the same process
	instance := session.str(13)
	if instance == "" {
		if target {
			return errors.New("matching audio session has no recoverable identity")
		}
		return nil
	}
	// Mute only this session through ISimpleAudioVolume
	volume, err := control.query(&iVolume)
	if err != nil {
		if target {
			return err
		}
		return nil
	}
	defer volume.release()
	var current uint32
	// Read GetMute so recovery can preserve the original state
	if err := volume.invoke(6, uintptr(unsafe.Pointer(&current))); err != nil {
		if target {
			return err
		}
		return nil
	}
	return fn(audioSession{identity{endpoint, instance, pid, start}, current != 0, label, func(muted bool) error {
		var flag uintptr
		if muted {
			flag = 1
		}
		// SetMute takes a Windows BOOL and an optional event context pointer
		return volume.invoke(5, flag, 0)
	}})
}

// Keep audio recovery independent of the application process
func Run(dir string, done <-chan struct{}, ready func() error, report func(error), status func(string)) error {
	path := filepath.Join(dir, "gsx-mute.json")
	// Portable copies still mute the same audio sessions, so share one mutex
	const name = `Local\GSXCommsGo.Mute`
	mutex, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(name))
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	defer windows.CloseHandle(mutex)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// A periodic recovery must never queue behind a live mute
	var wait uint32
	if done != nil {
		wait = 5000
	}
	// Windows mutex ownership stays with the thread that acquires it
	rc, err := windows.WaitForSingleObject(mutex, wait)
	if err != nil {
		return err
	}
	// An abandoned mutex still gives this thread ownership so it can recover the journal
	if rc == uint32(windows.WAIT_TIMEOUT) && done == nil {
		return nil
	}
	if rc != windows.WAIT_OBJECT_0 && rc != windows.WAIT_ABANDONED {
		return errors.New("another GSX mute request is still active")
	}
	defer windows.ReleaseMutex(mutex)
	api := windowsAudio{}
	// Recovery mode replays the journal without claiming a new mute
	if done == nil {
		return restore(path, api)
	}
	select {
	case <-done:
		return nil
	default:
	}
	// Recover the previous window once before adding any new journal entries
	if err := restore(path, api); err != nil {
		report(err)
	}
	return errors.Join(muteUntilEOF(path, api, done, ready, report, status), restore(path, api))
}

// Rescan while held because GSX can open its audio session after the first menu action
func muteUntilEOF(path string, api backend, done <-chan struct{}, ready func() error, report func(error), status func(string)) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var matched bool
	var lastError string
	var scanErr error
	for {
		select {
		case <-done:
			if !matched && scanErr == nil {
				status("no-match")
			}
			return scanErr
		default:
		}
		found, err := acquire(path, api)
		scanErr = err
		if err != nil && err.Error() != lastError {
			report(err)
			lastError = err.Error()
		}
		if found && !matched {
			matched = true
			status("matched")
		}
		if ready != nil {
			if err := ready(); err != nil {
				return fmt.Errorf("mute watchdog pipe failed: %w", err)
			}
			ready = nil
		}
		select {
		case <-done:
			// Stop before the next scan, even if the ticker is also ready
		case <-ticker.C:
		}
	}
}
