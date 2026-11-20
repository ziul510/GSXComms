// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"encoding/binary"
	"slices"
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
		{"Operating stairs", false, false, false},
		{"Undock jetway", true, true, false},
		{"Request to disconnect GPU", true, false, true},
		{"Open stairs", false, true, true},
		{"Service completed", true, false, true},
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

func TestProtectedAudioSurvivesOverflowAndInterruption(t *testing.T) {
	s := &state{ctx: t.Context()}
	for range 16 {
		s.queueAudio(playback{cue: "normal"}, false)
	}
	if !s.queueAudio(playback{cue: "gpu", stateChange: true}, false) ||
		!s.queueAudio(playback{cue: "error 1"}, true) ||
		!s.queueAudio(playback{cue: "error 2"}, true) {
		t.Fatal("protected cue was rejected")
	}
	var cues []string
	for _, request := range s.audioQueue {
		cues = append(cues, request.cue)
	}
	if !slices.Equal(cues, []string{"error 1", "error 2", "gpu"}) {
		t.Fatalf("wrong protected playback order: %v", cues)
	}
}
