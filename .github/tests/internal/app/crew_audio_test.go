// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"encoding/binary"
	"testing"
)

func TestCrewServiceTransitions(t *testing.T) {
	for _, test := range []struct {
		label            string
		was, next, known bool
	}{
		{"Operate stairs", false, true, true},
		{"Operate stairs", true, false, true},
		{"Disconnect GPU", true, false, true},
		{"Request jetway", false, true, true},
		{"Jetway unavailable", true, true, false},
	} {
		if next, known := serviceConnected(test.was, test.label); next != test.next || known != test.known {
			t.Errorf("%q from %v = %v, %v", test.label, test.was, next, known)
		}
	}
}

func TestCrewLoudnessGain(t *testing.T) {
	for _, test := range []struct {
		amplitude uint16
		gain      float64
	}{{0, 1}, {600, 2.5}, {3000, 1}, {12000, 0.5}} {
		pcm := binary.LittleEndian.AppendUint16(nil, test.amplitude)
		if gain := normalizationGain(pcm); gain != test.gain {
			t.Errorf("gain for %d = %f, want %f", test.amplitude, gain, test.gain)
		}
	}
}
