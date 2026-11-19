// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var mm = windows.NewLazySystemDLL("winmm.dll")

const defaultWaveDevice = 0xffffffff

func audioDevices(input bool) []string {
	prefix := "waveOut"
	if input {
		prefix = "waveIn"
	}
	num := mm.NewProc(prefix + "GetNumDevs")
	caps := mm.NewProc(prefix + "GetDevCapsW")
	count, _, _ := num.Call()
	out := make([]string, 0, count)
	for i := uintptr(0); i < count; i++ {
		var raw [128]byte
		r, _, _ := caps.Call(i, uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw)))
		if r != 0 {
			out = append(out, "")
			continue
		}
		name := unsafe.Slice((*uint16)(unsafe.Pointer(&raw[8])), 32)
		out = append(out, windows.UTF16ToString(name))
	}
	return out
}

func deviceNames(input bool) []string {
	out := []string{"Default"}
	for _, name := range audioDevices(input) {
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

func deviceID(input bool, name string) uintptr {
	if name != "Default" && name != "" {
		for i, candidate := range audioDevices(input) {
			if name == candidate {
				return uintptr(i)
			}
		}
	}
	return defaultWaveDevice
}

type waveFormat struct {
	tag, channels        uint16
	rate, bytesPerSecond uint32
	align, bits, extra   uint16
}

type waveHeader struct {
	data             *byte
	length, recorded uint32
	user             uintptr
	flags, loops     uint32
	next, reserved   uintptr
}

//go:uintptrescapes
func mmCall(proc *windows.LazyProc, args ...uintptr) error {
	if result, _, _ := proc.Call(args...); result != 0 {
		return fmt.Errorf("%s failed (%d)", proc.Name, result)
	}
	return nil
}

func openCaptureDevice(name string, id uintptr, allowFallback bool, open func(uintptr) error) error {
	if !allowFallback && id == defaultWaveDevice && name != "" && name != "Default" {
		return errors.New("selected microphone is unavailable")
	}
	err := open(id)
	if err == nil || id == defaultWaveDevice {
		return err
	}
	if !allowFallback {
		return fmt.Errorf("could not open selected microphone: %w", err)
	}
	return open(defaultWaveDevice)
}

func capture(ctx context.Context, name string, limit int, allowFallback bool) ([]byte, error) {
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
	id := deviceID(true, name)
	openDevice := func(id uintptr) error {
		return mmCall(open, uintptr(unsafe.Pointer(&handle)), id, uintptr(unsafe.Pointer(&format)), uintptr(event), 0, 0x50000)
	}
	err = openCaptureDevice(name, id, allowFallback, openDevice)
	if err != nil {
		return nil, err
	}
	defer mmCall(mm.NewProc("waveInClose"), handle)
	reset := mm.NewProc("waveInReset")
	prep := mm.NewProc("waveInPrepareHeader")
	unprep := mm.NewProc("waveInUnprepareHeader")
	add := mm.NewProc("waveInAddBuffer")
	var buffers [2][3200]byte
	var headers [2]waveHeader
	hs := unsafe.Sizeof(headers[0])
	var pins runtime.Pinner
	defer pins.Unpin()
	prepared := 0
	defer func() {
		_ = mmCall(reset, handle)
		for i := 0; i < prepared; i++ {
			_ = mmCall(unprep, handle, uintptr(unsafe.Pointer(&headers[i])), hs)
		}
	}()
	for i := range headers {
		h := &headers[i]
		h.data, h.length = &buffers[i][0], uint32(len(buffers[i]))
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
			if h.flags&1 == 0 {
				break
			}
			n := min(int(h.recorded), len(buffers[i]), limit*rate*2-len(pcm))
			if n > 0 {
				pcm = append(pcm, buffers[i][:n]...)
			}
			next = (next + 1) % len(headers)
			if active {
				if err := mmCall(add, handle, uintptr(unsafe.Pointer(h)), hs); err != nil {
					return nil, err
				}
			}
		}
	}
	return pcm, nil
}

func playPCM(ctx context.Context, device string, pcm []byte, rate int) error {
	if len(pcm) == 0 {
		return nil
	}
	if rate < 8000 || rate > 192000 || len(pcm)%2 != 0 {
		return errors.New("invalid playback format")
	}
	format := waveFormat{tag: 1, channels: 1, rate: uint32(rate), bytesPerSecond: uint32(rate * 2), align: 2, bits: 16}
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
	id := deviceID(false, device)
	err = openDevice(id)
	if err != nil && id != defaultWaveDevice {
		err = openDevice(defaultWaveDevice)
	}
	if err != nil {
		return err
	}
	defer mmCall(mm.NewProc("waveOutClose"), handle)
	h := waveHeader{data: &pcm[0], length: uint32(len(pcm))}
	var pins runtime.Pinner
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
	for h.flags&1 == 0 {
		if ctx.Err() != nil {
			_ = mmCall(mm.NewProc("waveOutReset"), handle)
			return ctx.Err()
		}
		status, err := windows.WaitForSingleObject(event, 25)
		if err != nil || status == 0xffffffff {
			return errors.Join(err, syscall.EINVAL)
		}
	}
	return nil
}
