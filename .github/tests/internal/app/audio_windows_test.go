// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestAudioEndpointMatching(t *testing.T) {
	candidates := []audioDevice{{"Headset", "one"}, {"Headset", "two"}, {"Headset Studio", "three"}, {"Desk Mic", "four"}}
	for _, test := range []struct {
		name, endpoint string
		want           uintptr
	}{
		{"Headset", "two", 1},
		{"renamed", "three", 2},
		{"Headset Studio", "gone", 2},
		{"Desk", "", 3},
		{"Headset", "", defaultWaveDevice},
		{"set", "", defaultWaveDevice},
		{"", "", defaultWaveDevice},
	} {
		if id := matchAudioDevice(test.name, test.endpoint, candidates, true); id != test.want {
			t.Errorf("%q / %q resolved to %d, want %d", test.name, test.endpoint, id, test.want)
		}
	}
}

func TestOpenCaptureDeviceFallback(t *testing.T) {
	prefix := strings.Repeat("x", 31)
	candidates := []audioDevice{
		{"Headset", "headset"}, {"Busy Mic", "busy"}, {"Desk Mic", "desk"}, {prefix, "truncated"},
		{"USB Mic", "usb-one"}, {"USB Mic", "usb-two"}, {"Default", "literal-default"},
	}
	for _, tc := range []struct {
		label, name, endpoint string
		allowFallback         bool
		wantedError           bool
		attempts              []uintptr
	}{
		{"cloud missing", "Missing Mic", "", false, true, nil},
		{"cloud missing endpoint with exact name", "Headset", "missing", false, true, nil},
		{"cloud missing endpoint with default name", "Default", "missing", false, true, nil},
		{"cloud missing endpoint with empty name", "", "missing", false, true, nil},
		{"cloud partial", "Desk", "", false, true, nil},
		{"cloud truncated", prefix + " original microphone", "", false, true, nil},
		{"cloud ambiguous", "USB Mic", "", false, true, nil},
		{"cloud busy", "Busy Mic", "busy", false, true, []uintptr{1}},
		{"cloud legacy exact name", "headset", "", false, false, []uintptr{0}},
		{"cloud renamed endpoint", "Old Desk Mic", "desk", false, false, []uintptr{2}},
		{"cloud endpoint with default name", "Default", "literal-default", false, false, []uintptr{6}},
		// Cloud capture can still use the system device if the settings select it
		{"cloud default", "Default", "", false, false, []uintptr{defaultWaveDevice}},
		{"cloud legacy default", "", "", false, false, []uintptr{defaultWaveDevice}},
		{"local partial", "Desk", "", true, false, []uintptr{2}},
		{"local missing", "Missing Mic", "", true, false, []uintptr{defaultWaveDevice}},
		{"local busy", "Busy Mic", "busy", true, false, []uintptr{1, defaultWaveDevice}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var attempts []uintptr
			err := openCaptureDevice(tc.name, tc.endpoint, candidates, tc.allowFallback, func(id uintptr) error {
				attempts = append(attempts, id)
				// Simulate a busy named device without opening the host microphone
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
