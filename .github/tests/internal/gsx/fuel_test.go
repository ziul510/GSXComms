// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"context"
	"testing"
	"time"
)

// A GSX choice on the fuel page must wait for the pilot, like the percentage rows.
func TestAutoOperatorLeavesFuelQuantityPrompt(t *testing.T) {
	sim := &fakeSim{values: make(map[string]float64)}
	session := New(sim, "")
	current := ParseMenu("Ramp\nRequest refueling")
	quantity := ParseMenu("Select refueling level\nGSX Choice\n50%\n100%")
	session.ReadMenu = func() Menu { return current }
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	choices := 0
	sim.onWrite = func(key string, value float64) {
		if key == MenuChoice {
			choices++
			current = quantity
		}
	}
	if _, err := session.Execute(t.Context(), "request refueling", "REFUELING", []string{"fuel"}, true); err != nil {
		t.Fatal(err)
	}
	if choices != 1 || !current.Same(quantity) {
		t.Fatalf("sent %d choices, want only the refueling request", choices)
	}
}
