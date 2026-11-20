// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestHelperTimesOutWithoutCallerDeadline(t *testing.T) {
	const child = "GSXCOMMS_TEST_STALLED_HELPER"
	if os.Getenv(child) == "1" {
		// Complete the handshake, then hold both pipes open without replying.
		fmt.Println("{}")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHelperTimesOutWithoutCallerDeadline$")
	cmd.Env = append(cmd.Environ(), child+"=1")
	transport, err := connectHelper(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	// Keep the regression test bounded even if the transport deadline is removed.
	cleanup := time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
	defer cleanup.Stop()
	_, err = transport.Get(context.Background(), Started)
	if !errors.Is(err, ErrDisconnected) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled helper returned %v", err)
	}
}
