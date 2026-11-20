// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveSimConnect(t *testing.T) {
	if os.Getenv("GSXCOMMS_TEST_LIVE_MSFS") != "1" {
		t.Skip("enable with GSXCOMMS_TEST_LIVE_MSFS=1 while MSFS is running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sim, err := Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()
	session := New(sim, Panel())
	ready, speed, err := session.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SimConnect handshake succeeded. GSX ready=%v, groundspeed=%.1f kt", ready, speed)
	if !ready {
		t.Skip("GSX readiness requires a loaded flight")
	}
}
