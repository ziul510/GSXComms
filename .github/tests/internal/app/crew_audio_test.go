// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/binary"
	"slices"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
)

// A slow watchdog must never stall menu work or delay playback beyond its budget
func TestCrewMuteDoesNotBlockMenuOrPlayback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var m crewMute
		started := make(chan struct{})
		finished := make(chan struct{})
		start := time.Now()
		ready, releaseMenu := m.hold(t.Context(), 20*time.Second, func(ctx context.Context, _ func()) {
			close(started)
			<-ctx.Done()
			close(finished)
		})
		<-started
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("menu blocked for %s", elapsed)
		}
		releaseMenu()
		reuseWatchdog := func(context.Context, func()) {
			t.Error("overlapping audio started a second watchdog")
		}
		playbackReady, releaseAudio := m.hold(t.Context(), 3950*time.Millisecond, reuseWatchdog)
		if playbackReady != ready {
			t.Fatal("menu and playback should share the pending watchdog")
		}
		waitForMute(t.Context(), playbackReady)
		if elapsed := time.Since(start); elapsed != 100*time.Millisecond {
			t.Fatalf("playback blocked for %s", elapsed)
		}
		time.Sleep(4 * time.Second)
		releaseAudio()
		// Another choice must restart the 20-second window on the same watchdog.
		_, releaseNext := m.hold(t.Context(), 20*time.Second, reuseWatchdog)
		releaseNext()
		// A shorter clip must leave the later deadline in place.
		_, releaseClip := m.hold(t.Context(), 3950*time.Millisecond, reuseWatchdog)
		releaseClip()
		<-finished
		if elapsed := time.Since(start); elapsed != 24100*time.Millisecond {
			t.Fatalf("watchdog stopped at %s", elapsed)
		}
	})
}

// Only predefined diagnostics may reach logs that users can share
func TestSafeWatchdogDiagnostic(t *testing.T) {
	for input, want := range map[string]string{
		"Core Audio method 3 failed (0x80004002)":                 "Core Audio method 3 failed (0x80004002)",
		"another GSX mute request is still active":                "another GSX mute request is still active",
		`write mute journal: open C:\Users\private\gsx-mute.json`: "write mute journal",
		"mute journal has invalid JSON: secret":                   "mute journal has invalid JSON",
		"unexpected private child output":                         "",
	} {
		if got := safeWatchdogDiagnostic(input); got != want {
			t.Errorf("safeWatchdogDiagnostic(%q) = %q, want %q", input, got, want)
		}
	}
}

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
		{"Undock jetway", true, false, true},
		{"Request to disconnect GPU", true, false, true},
		{"Open stairs", false, true, true},
		{"Service completed", true, false, true},
	} {
		// Check whether the label was understood as well as whether the service state changed.
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
		// Use one sample per case so the input RMS equals the fixture amplitude.
		pcm := binary.LittleEndian.AppendUint16(nil, test.amplitude)
		if gain := normalizationGain(pcm); gain != test.gain {
			t.Errorf("gain for %d = %f, want %f", test.amplitude, gain, test.gain)
		}
	}
}

func TestProtectedAudioSurvivesOverflowAndInterruption(t *testing.T) {
	s := &state{ctx: t.Context()}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{})
	if !s.queueAudio(playback{readout: "readout fixture", ordered: true}, false) {
		t.Fatal("readout was rejected with space in the queue")
	}
	// Fill the remaining slots with ordinary cues that an interruption can drop.
	for range 15 {
		s.queueAudio(playback{cue: "normal"}, false)
	}
	// Readouts still respect capacity even though queued readouts survive interruptions.
	if s.queueAudio(playback{readout: "overflow", ordered: true}, false) {
		t.Fatal("readout exceeded queue capacity")
	}
	if !s.queueAudio(playback{cue: "gpu", ordered: true}, false) ||
		!s.queueAudio(playback{cue: "error 1"}, true) ||
		!s.queueAudio(playback{cue: "error 2"}, true) {
		t.Fatal("protected cue was rejected")
	}
	var cues []string
	for _, request := range s.audioQueue {
		cues = append(cues, request.cue)
	}
	if !slices.Equal(cues, []string{"error 1", "error 2", "", "gpu"}) || s.audioQueue[2].readout != "readout fixture" {
		t.Fatalf("wrong protected playback order: %v", cues)
	}
}

func TestPlaybackQueueCapsAndCoalescesPriority(t *testing.T) {
	s := &state{ctx: t.Context()}
	for range 30 {
		if !s.queueAudio(playback{cue: "error"}, true) {
			t.Fatal("priority error was rejected")
		}
	}
	if len(s.audioQueue) != 1 {
		t.Fatalf("duplicate priority cues = %d, want 1", len(s.audioQueue))
	}
	for i := range 30 {
		s.queueAudio(playback{cue: "error", option: strconv.Itoa(i)}, true)
	}
	if len(s.audioQueue) != 16 || s.audioQueue[15].option != "29" {
		t.Fatalf("queue length = %d, wanted 16 including the latest urgent cue", len(s.audioQueue))
	}
}

func TestDisablingMenuReaderDiscardsQueuedReadouts(t *testing.T) {
	s := &state{ctx: t.Context(), cfg: config.Defaults()}
	s.cfg.Put(config.MenuReadoutSetting, true)
	s.readouts = newReadoutScope(s.ctx)
	previous := s.readouts
	previous.speaking <- struct{}{}
	if !s.queueAudio(playback{dry: true, ordered: true, readout: "old"}, false) {
		t.Fatal("fixture did not queue")
	}
	disabled := s.cfg.Copy()
	disabled.Put(config.MenuReadoutSetting, false)
	s.finishSave(&saveRequest{values: disabled}, nil)
	enabled := disabled.Copy()
	enabled.Put(config.MenuReadoutSetting, true)
	s.finishSave(&saveRequest{values: enabled}, nil)
	if len(s.audioQueue) != 0 || s.queueAudio(playback{dry: true, pcm: []byte{0, 0}}, false) ||
		previous.ctx.Err() == nil || s.readouts.ctx.Err() != nil || len(s.readouts.speaking) != 0 {
		t.Fatal("previous-generation readout survived disabling the reader")
	}
}

func TestQueueOverflowKeepsLatestStateForEachService(t *testing.T) {
	s := &state{ctx: t.Context()}
	for i := range 16 {
		s.queueAudio(playback{cue: "jetway", option: strconv.Itoa(i), ordered: true}, false)
	}
	if !s.queueAudio(playback{cue: "gpu", ordered: true}, false) {
		t.Fatal("the queue discarded a new service transition")
	}
	if len(s.audioQueue) != 2 || s.audioQueue[0].cue != "jetway" || s.audioQueue[0].option != "15" || s.audioQueue[1].cue != "gpu" {
		t.Fatalf("queue did not retain the latest state for each service: %+v", s.audioQueue)
	}
}

func TestMuteShutdownWaitsForRetiredWatchdog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var m crewMute
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan struct{})
		_, release := m.hold(ctx, 0, func(ctx context.Context, _ func()) {
			close(started)
			<-ctx.Done()
			time.Sleep(2 * time.Second)
		})
		<-started
		release()
		synctest.Wait()
		cancel()
		shutdown, stop := context.WithTimeout(t.Context(), 2500*time.Millisecond)
		defer stop()
		start := time.Now()
		m.wait(shutdown)
		if elapsed := time.Since(start); elapsed != 2*time.Second {
			t.Fatalf("shutdown waited %s for watchdog", elapsed)
		}
	})
}
