// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ziul510/GSXCommsGo/internal/config"
)

func TestPTTBindingChangeDiscardsCapture(t *testing.T) {
	previousKey := captureKey.Load()
	t.Cleanup(func() { captureKey.Store(previousKey) })
	s := &state{ctx: t.Context(), cfg: config.Defaults(), pttHeld: true}
	capture, stop := context.WithCancelCause(s.ctx)
	defer stop(nil)
	s.recording = stop
	updated := s.cfg.Copy()
	updated.Put("ptt_bind", bindingForKey(0x41))
	s.finishSave(&saveRequest{values: updated}, nil)
	if s.pttHeld || context.Cause(capture) != errPTTBindingChanged {
		t.Fatalf("binding change left held=%v with capture cause %v", s.pttHeld, context.Cause(capture))
	}

	// A real release is the only event that authorizes transcription.
	capture, stop = context.WithCancelCause(s.ctx)
	defer stop(nil)
	s.recording = stop
	s.record(false)
	if context.Cause(capture) != errPTTReleased {
		t.Fatalf("button release cause: %v", context.Cause(capture))
	}
}

func TestWaitForPTTStatic(t *testing.T) {
	// Use a fake clock so scheduler delays cannot hide a one millisecond regression.
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		// Release one millisecond before the threshold to catch an early static cue.
		shortPress, release := context.WithTimeout(t.Context(), 274*time.Millisecond)
		defer release()
		if waitForPTTStatic(shortPress) || time.Since(start) != 274*time.Millisecond {
			t.Fatal("releasing at 274 ms must cancel static immediately")
		}

		start = time.Now()
		if !waitForPTTStatic(t.Context()) || time.Since(start) != 275*time.Millisecond {
			t.Fatal("a held button must allow static at 275 ms")
		}
	})
}
