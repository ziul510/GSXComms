// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestProtocol(t *testing.T) {
	f := Frame{Version: Version, ID: 5, Op: "transcribe", SampleRate: 16000, PCM: []byte{0, 128, 0, 0}}
	var stream bytes.Buffer
	// Round-trip a frame containing binary audio as well as metadata.
	if err := Write(&stream, f); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&stream)
	if err != nil || !reflect.DeepEqual(f, got) {
		t.Fatalf("frame = %+v, %v", got, err)
	}
	// Feed only the invalid size to verify rejection before the payload is read.
	_ = binary.Write(&stream, binary.LittleEndian, uint32(MaxFrame+1))
	if _, err := Read(&stream); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestSilenceGate(t *testing.T) {
	audio := make([]byte, 8000)
	if Usable(audio) {
		t.Fatal("silent audio passed")
	}
	// The most negative PCM16 value still has to pass the loudness gate.
	audio[1] = 128
	if !Usable(audio) {
		t.Fatal("full-scale negative audio discarded")
	}
	if Usable(audio[:7998]) {
		t.Fatal("short audio passed")
	}
}

func TestWorkerRequiresExplicitRetryAfterRepeatedFailures(t *testing.T) {
	w := NewWorker("missing-test-worker")
	if err := w.Load(t.Context(), "model", nil, nil); err == nil || w.failures != 1 {
		t.Fatalf("first launch: failures=%d err=%v", w.failures, err)
	}
	w.failed = time.Now().Add(-2 * time.Second)
	if err := w.Load(t.Context(), "model", nil, nil); err == nil || w.failures != 2 {
		t.Fatalf("second launch: failures=%d err=%v", w.failures, err)
	}
	// Elapsed time alone can't clear the explicit reload requirement.
	w.failed = time.Now().Add(-2 * time.Minute)
	if err := w.start(t.Context()); err == nil || w.failures != 2 {
		t.Fatalf("unexpected automatic retry: failures=%d err=%v", w.failures, err)
	}
	w.Reset()
	if w.failures != 0 || !w.failed.IsZero() {
		t.Fatal("reset did not reenable worker startup")
	}
}

func TestCanceledExchangeSkipsDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// The canceled context should return before any child or pipe is needed.
	w := NewWorker("sherpa")
	if _, err := w.exchange(ctx, time.Second, Frame{Op: "transcribe"}); !errors.Is(err, context.Canceled) || w.sequence != 0 {
		t.Fatalf("sequence=%d err=%v", w.sequence, err)
	}
}
