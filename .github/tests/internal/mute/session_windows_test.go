// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package mute

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRetainedNotificationSessionIsRevisited(t *testing.T) {
	var visits int
	vtable := &[32]uintptr{}
	// Track attempts to query the retained control, with no native audio changes.
	vtable[0] = windows.NewCallback(func(_ *com, _ *windows.GUID, _ **com) uintptr {
		visits++
		return 0x80004002 // E_NOINTERFACE
	})
	control := &com{vtable: vtable}
	notifications := &sessionNotifications{held: map[identity]notifiedSession{
		{Endpoint: "fixture"}: {control: control, endpoint: "fixture"},
	}}
	for _, closed := range []bool{false, true} {
		notifications.closed = closed
		before := visits
		if err := notifications.drain(func(audioSession) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if visits != before+1 {
			t.Errorf("held control was not revisited with closed=%v", closed)
		}
	}
}

// Activate each render endpoint's session manager without changing audio
func TestLiveAudioSessionEnumeration(t *testing.T) {
	// Headless runners may have no render device on which to activate the manager
	if os.Getenv("GSXCOMMS_TEST_LIVE_AUDIO") != "1" {
		t.Skip("enable with GSXCOMMS_TEST_LIVE_AUDIO=1 on Windows with an active render device")
	}
	if err := (windowsAudio{}).Visit(func(audioSession) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

type lateAudio struct {
	*mock
	visible atomic.Bool
}

// Expose the session only after the watchdog has sent ready
func (a *lateAudio) Visit(visit func(audioSession) error) error {
	if !a.visible.Load() {
		return nil
	}
	return a.mock.Visit(visit)
}

// A session created during the hold must be muted and restored on EOF
func TestWatchdogMutesLateSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gsx-mute.json")
	audio := &lateAudio{mock: &mock{id: identity{"endpoint", "instance", 35, 9}}}
	done := make(chan struct{})
	finished := make(chan error, 1)
	matched := make(chan struct{}, 1)
	go func() {
		finished <- muteUntilEOF(path, audio, done, func() error {
			audio.visible.Store(true)
			return nil
		}, func(err error) { t.Error(err) }, func(status string) {
			if status == "matched" {
				matched <- struct{}{}
			}
		})
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-matched:
		close(done)
	case <-timer.C:
		close(done)
		<-finished
		t.Fatal("watchdog missed the late session")
	}
	if err := <-finished; err != nil || !audio.muted {
		t.Fatalf("hold ended with muted=%v err=%v", audio.muted, err)
	}
	if err := restore(path, audio); err != nil || audio.muted {
		t.Fatalf("restore ended with muted=%v err=%v", audio.muted, err)
	}
}

// A broken session must not prevent muting another session in the same scan
func TestAcquireContinuesAfterSessionError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gsx-mute.json")
	first := identity{"endpoint", "first", 35, 9}
	second := identity{"endpoint", "second", 36, 10}
	var muted bool
	api := sessions{
		{ID: first, Label: "couatl64_boot.exe", Set: func(bool) error { return os.ErrPermission }},
		{ID: second, Label: "couatl64_MSFS.exe", Set: func(value bool) error { muted = value; return nil }},
		{ID: identity{"endpoint", "already muted", 37, 11}, Label: "Couatl", Muted: true},
		{Label: "GSXComms.exe"},
		{Label: "FlightSimulator2024.exe"},
	}
	if found, err := acquire(path, api); err == nil || !found || !muted {
		t.Fatalf("acquire ended with found=%v muted=%v err=%v", found, muted, err)
	}
}
