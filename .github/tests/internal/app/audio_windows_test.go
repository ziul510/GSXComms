// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"errors"
	"slices"
	"testing"
)

func TestOpenCaptureDeviceFallback(t *testing.T) {
	for _, tc := range []struct {
		label, name   string
		id            uintptr
		allowFallback bool
		wantedError   bool
		attempts      []uintptr
	}{
		{"cloud missing", "Headset", defaultWaveDevice, false, true, nil},
		{"cloud busy", "Headset", 1, false, true, []uintptr{1}},
		{"cloud selected", "Headset", 0, false, false, []uintptr{0}},
		// Cloud capture can still use the system device if the settings select it.
		{"cloud default", "Default", defaultWaveDevice, false, false, []uintptr{defaultWaveDevice}},
		{"cloud legacy default", "", defaultWaveDevice, false, false, []uintptr{defaultWaveDevice}},
		{"local missing", "Headset", defaultWaveDevice, true, false, []uintptr{defaultWaveDevice}},
		{"local busy", "Headset", 1, true, false, []uintptr{1, defaultWaveDevice}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var attempts []uintptr
			err := openCaptureDevice(tc.name, tc.id, tc.allowFallback, func(id uintptr) error {
				attempts = append(attempts, id)
				// Simulate a busy named device without opening the host microphone.
				if id == 1 {
					return errors.New("device busy")
				}
				return nil
			})
			if !slices.Equal(attempts, tc.attempts) || (err != nil) != tc.wantedError {
				t.Fatalf("attempts %v, error %v. Want attempts %v, error present %v", attempts, err, tc.attempts, tc.wantedError)
			}
		})
	}
}
