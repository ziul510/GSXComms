// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var mm = windows.NewLazySystemDLL("winmm.dll")

// WinMM uses WAVE_MAPPER to resolve the system default endpoint
const defaultWaveDevice = 0xffffffff

type audioDevice struct {
	name, endpoint string
}

func audioDevices(input bool) []audioDevice {
	prefix := "waveOut"
	if input {
		prefix = "waveIn"
	}
	num := mm.NewProc(prefix + "GetNumDevs")
	caps := mm.NewProc(prefix + "GetDevCapsW")
	message := mm.NewProc(prefix + "Message")
	count, _, _ := num.Call()
	out := make([]audioDevice, count)
	for i := range out {
		var raw [128]byte
		if err := mmCall(caps, uintptr(i), uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw))); err == nil {
			name := unsafe.Slice((*uint16)(unsafe.Pointer(&raw[8])), 32)
			out[i].name = windows.UTF16ToString(name)
			out[i].endpoint = waveEndpointID(message, uintptr(i))
		}
	}
	return out
}

func waveEndpointID(message *windows.LazyProc, device uintptr) string {
	// The endpoint ID buffer holds UTF16 code units
	const queryID, querySize = 0x0800 + 17, 0x0800 + 18
	var size uint32
	if err := mmCall(message, device, querySize, uintptr(unsafe.Pointer(&size)), 0); err != nil || size < 2 || size > 8192 || size%2 != 0 {
		return ""
	}
	buf := make([]uint16, size/2)
	if err := mmCall(message, device, queryID, uintptr(unsafe.Pointer(&buf[0])), uintptr(size)); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf)
}

func defaultAudioSelection(name, endpoint string) bool {
	return endpoint == "" && (name == "" || name == "Default")
}

func matchAudioDevice(name, endpoint string, candidates []audioDevice, allowFallback bool) uintptr {
	if allowFallback {
		name = strings.ToLower(strings.TrimSpace(name))
	}
	match := func(matches func(audioDevice) bool) (uintptr, bool) {
		id := uintptr(defaultWaveDevice)
		for i, candidate := range candidates {
			if matches(candidate) {
				if id != defaultWaveDevice {
					return defaultWaveDevice, true
				}
				id = uintptr(i)
			}
		}
		return id, id != defaultWaveDevice
	}
	if endpoint != "" {
		// Cloud capture requires the saved endpoint when one was selected
		if id, found := match(func(device audioDevice) bool { return strings.EqualFold(device.endpoint, endpoint) }); found || !allowFallback {
			return id
		}
	}
	if name == "" || name == "Default" || allowFallback && name == "default" {
		return defaultWaveDevice
	}
	if id, found := match(func(device audioDevice) bool { return strings.EqualFold(device.name, name) }); found || !allowFallback {
		return id
	}
	id, _ := match(func(device audioDevice) bool {
		label := strings.ToLower(device.name)
		return label != "" && (strings.Contains(label, name) || strings.Contains(name, label))
	})
	return id
}

// The calls pass the native WAVEFORMATEX field layout for mono PCM16
type waveFormat struct {
	tag, channels        uint16
	rate, bytesPerSecond uint32
	align, bits, extra   uint16
}

// WinMM updates the byte count and flags in this WAVEHDR while the buffer is queued
type waveHeader struct {
	data             *byte
	length, recorded uint32
	user             uintptr
	flags, loops     uint32
	next, reserved   uintptr
}

type waveError struct {
	operation string
	code      uintptr
}

func (err waveError) Error() string { return fmt.Sprintf("%s failed (%d)", err.operation, err.code) }

//go:uintptrescapes
func mmCall(proc *windows.LazyProc, args ...uintptr) error {
	// WinMM APIs report MMRESULT directly Zero is success
	if result, _, _ := proc.Call(args...); result != 0 {
		return waveError{proc.Name, result}
	}
	return nil
}

var errMicUnavailable = errors.New("selected microphone is unavailable")

func openCaptureDevice(name, endpoint string, candidates []audioDevice, allowFallback bool, open func(uintptr) error) error {
	id := matchAudioDevice(name, endpoint, candidates, allowFallback)
	// WAVE_MAPPER is safe for cloud capture only when the settings select it
	if !allowFallback && id == defaultWaveDevice && !defaultAudioSelection(name, endpoint) {
		return errMicUnavailable
	}
	err := open(id)
	if err == nil || id == defaultWaveDevice {
		return err
	}
	// Cloud capture also rejects a named device that exists but won't open
	if !allowFallback {
		return fmt.Errorf("could not open selected microphone: %w", err)
	}
	return open(defaultWaveDevice)
}

func capture(ctx context.Context, name, endpoint string, limit int, allowFallback bool, fallback func()) ([]byte, error) {
	// Both transcription providers consume 16 kHz mono samples
	const rate = 16000
	limit = max(10, min(120, limit))
	format := waveFormat{tag: 1, channels: 1, rate: rate, bytesPerSecond: rate * 2, align: 2, bits: 16}
	var handle uintptr
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(event)
	open := mm.NewProc("waveInOpen")
	var actual uintptr
	// CALLBACK_EVENT lets WinMM wake capture as buffers return
	openDevice := func(id uintptr) error {
		actual = id
		return mmCall(open, uintptr(unsafe.Pointer(&handle)), id, uintptr(unsafe.Pointer(&format)), uintptr(event), 0, 0x50000)
	}
	err = openCaptureDevice(name, endpoint, audioDevices(true), allowFallback, openDevice)
	if err != nil {
		return nil, err
	}
	if actual == defaultWaveDevice && !defaultAudioSelection(name, endpoint) && fallback != nil {
		fallback()
	}
	defer mmCall(mm.NewProc("waveInClose"), handle)
	reset := mm.NewProc("waveInReset")
	prep := mm.NewProc("waveInPrepareHeader")
	unprep := mm.NewProc("waveInUnprepareHeader")
	add := mm.NewProc("waveInAddBuffer")
	// Two buffers let capture continue while Go drains a finished buffer
	var buffers [2][3200]byte
	var headers [2]waveHeader
	hs := unsafe.Sizeof(headers[0])
	var pins runtime.Pinner
	defer pins.Unpin()
	prepared := 0
	// Return queued buffers before unpreparing the headers and releasing pinned memory
	defer func() {
		_ = mmCall(reset, handle)
		for i := 0; i < prepared; i++ {
			_ = mmCall(unprep, handle, uintptr(unsafe.Pointer(&headers[i])), hs)
		}
	}()
	for i := range headers {
		h := &headers[i]
		h.data, h.length = &buffers[i][0], uint32(len(buffers[i]))
		// WinMM keeps both pointers after waveInAddBuffer returns
		pins.Pin(h)
		pins.Pin(h.data)
		ptr := uintptr(unsafe.Pointer(h))
		if err := mmCall(prep, handle, ptr, hs); err != nil {
			return nil, err
		}
		prepared++
		if err := mmCall(add, handle, ptr, hs); err != nil {
			return nil, err
		}
	}
	if err := mmCall(mm.NewProc("waveInStart"), handle); err != nil {
		return nil, err
	}
	pcm := make([]byte, 0, rate*2*min(limit, 10))
	t := time.Now().Add(time.Duration(limit) * time.Second)
	active := true
	next := 0
	for active {
		// Reset returns the partial buffer so releasing PTT keeps the tail of the recording
		if ctx.Err() != nil || time.Now().After(t) {
			_ = mmCall(reset, handle)
			active = false
		} else {
			if _, err := windows.WaitForSingleObject(event, 25); err != nil {
				return nil, err
			}
		}
		for count := 0; count < len(headers); count++ {
			i := next
			h := &headers[i]
			// WHDR_DONE marks a buffer returned by the driver Drain in the order queued
			if h.flags&1 == 0 {
				break
			}
			// Use the recorded count because the last returned buffer may be partial
			n := min(int(h.recorded), len(buffers[i]), limit*rate*2-len(pcm))
			if n > 0 {
				pcm = append(pcm, buffers[i][:n]...)
			}
			next = (next + 1) % len(headers)
			// Reuse a drained header only while capture is still running
			if active {
				if err := mmCall(add, handle, uintptr(unsafe.Pointer(h)), hs); err != nil {
					return nil, err
				}
			}
		}
	}
	return pcm, nil
}

func playPCMBuffer(ctx context.Context, device, endpoint string, pcm []byte, rate, channels int, loop bool) error {
	if len(pcm) == 0 {
		return nil
	}
	if channels < 1 || channels > 2 || rate < 8000 || rate > 192000 || len(pcm)%(2*channels) != 0 {
		return errors.New("invalid playback format")
	}
	align := uint16(2 * channels)
	format := waveFormat{tag: 1, channels: uint16(channels), rate: uint32(rate), bytesPerSecond: uint32(rate) * uint32(align), align: align, bits: 16}
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(event)
	var handle uintptr
	open := mm.NewProc("waveOutOpen")
	openDevice := func(id uintptr) error {
		return mmCall(open, uintptr(unsafe.Pointer(&handle)), id, uintptr(unsafe.Pointer(&format)), uintptr(event), 0, 0x50000)
	}
	id := matchAudioDevice(device, endpoint, audioDevices(false), true)
	err = openDevice(id)
	// Playback can use the default speakers if the selected output fails
	if err != nil && id != defaultWaveDevice {
		err = openDevice(defaultWaveDevice)
	}
	if err != nil {
		return err
	}
	defer mmCall(mm.NewProc("waveOutClose"), handle)
	h := waveHeader{data: &pcm[0], length: uint32(len(pcm))}
	if loop {
		h.flags = 0x04 | 0x08 // WHDR_BEGINLOOP and WHDR_ENDLOOP
		h.loops = ^uint32(0)
	}
	var pins runtime.Pinner
	// Keep the output header and samples fixed until the asynchronous write is done
	pins.Pin(&h)
	pins.Pin(h.data)
	defer pins.Unpin()
	hs, ptr := unsafe.Sizeof(h), uintptr(unsafe.Pointer(&h))
	if err := mmCall(mm.NewProc("waveOutPrepareHeader"), handle, ptr, hs); err != nil {
		return err
	}
	defer mmCall(mm.NewProc("waveOutUnprepareHeader"), handle, ptr, hs)
	if err := mmCall(mm.NewProc("waveOutWrite"), handle, ptr, hs); err != nil {
		return err
	}
	defer func() {
		if h.flags&1 == 0 {
			_ = mmCall(mm.NewProc("waveOutReset"), handle)
		}
	}()
	for h.flags&1 == 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		status, err := windows.WaitForSingleObject(event, 25)
		if err != nil || status == 0xffffffff {
			return errors.Join(err, syscall.EINVAL)
		}
	}
	return nil
}
