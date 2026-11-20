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
		t.Skip("set GSXCOMMS_TEST_ASR_MODEL to a verified model directory beside the test package")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	w := NewWorker("sherpa")
	defer w.Reset()
	vocab, err := commands.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	for _, name := range []string{"encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt"} {
		paths[name] = filepath.Join(dir, name)
	}
	if err := w.Load(ctx, "restart-fixture", paths, vocab.Hotwords()); err != nil {
		t.Fatal(err)
	}
	first := w.cmd.Process.Pid
	// Kill the child outside Worker so the next exchange has to detect the crash
	if err := w.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Transcribe(ctx, make([]byte, 8000)); err == nil {
		t.Fatal("crashed worker still looked usable")
	}
	if err := w.Load(ctx, "restart-fixture", paths, vocab.Hotwords()); err != nil {
		t.Fatal(err)
	}
	if w.cmd.Process.Pid == first {
		t.Fatal("worker did not restart")
	}
	// Silent PCM exercises the replacement worker without needing a spoken fixture
	if text, err := w.Transcribe(ctx, make([]byte, 8000)); err != nil || text != "" {
		t.Fatalf("replacement recognition = %q, %v", text, err)
	}
}
