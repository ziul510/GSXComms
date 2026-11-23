// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package speech

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ziul510/GSXCommsGo/internal/commands"
)

func TestInstalledWorkerRestarts(t *testing.T) {
	dir := os.Getenv("GSXCOMMS_TEST_ASR_MODEL")
	if dir == "" {
		t.Skip("set GSXCOMMS_TEST_ASR_MODEL to an absolute verified model directory")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	w := NewWorker("sherpa")
	workerPath := os.Getenv("GSXCOMMS_TEST_ASR_WORKER")
	if workerPath == "" {
		workerPath = filepath.Join("..", "..", "build", "workers", "sherpa", "gsxspeech.exe")
	}
	var err error
	w.workerPath, err = filepath.Abs(workerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Reset()
	vocab, err := commands.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	for _, name := range []string{"encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt"} {
		paths[name] = filepath.Join(dir, name)
	}
	if err := w.Load(ctx, "restart-fixture", paths, vocab.Hotwords(), nil); err != nil {
		t.Fatal(err)
	}
	first := w.cmd.Process.Pid
	// Kill the child outside Worker so the next exchange has to detect the crash
	if err := w.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Make a real exchange after the kill so the worker detects the broken pipe.
	if _, err := w.Transcribe(ctx, make([]byte, 8000)); err == nil {
		t.Fatal("crashed worker still looked usable")
	}
	if err := w.Load(ctx, "restart-fixture", paths, vocab.Hotwords(), nil); err != nil {
		t.Fatal(err)
	}
	// Check the process ID because a cached model signature alone cannot prove the restart.
	if w.cmd.Process.Pid == first {
		t.Fatal("worker did not restart")
	}
	// Silent PCM exercises the replacement worker without needing a spoken fixture
	if text, err := w.Transcribe(ctx, make([]byte, 8000)); err != nil || text != "" {
		t.Fatalf("replacement recognition = %q, %v", text, err)
	}
}
