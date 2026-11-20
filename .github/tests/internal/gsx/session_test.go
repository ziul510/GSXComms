// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

type operation struct {
	key   string
	value float64
}
type fakeSim struct {
	values  map[string]float64
	writes  []operation
	closed  bool
	onWrite func(string, float64)
	failGet int
	missing string
}

func (f *fakeSim) Get(ctx context.Context, key string) (float64, error) {
	// Simulate a single unavailable variable without taking the whole transport offline.
	if key == f.missing {
		return 0, errors.New("unavailable simulator variable")
	}
	// Inject transient reads so the script also exercises the retry loop.
	if f.failGet > 0 {
		f.failGet--
		return 0, errors.New("injected read failure")
	}
	return f.values[key], ctx.Err()
}
func (f *fakeSim) Set(ctx context.Context, key string, value float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.values[key] = value
	// Record every flag and choice so tests can verify protocol order.
	f.writes = append(f.writes, operation{key, value})
	// A choice can advance the fake panel like the real simulator would.
	if f.onWrite != nil {
		f.onWrite(key, value)
	}
	return nil
}
func (f *fakeSim) Close() error { f.closed = true; return nil } // Track whether cleanup actually closed the transport.

func TestScriptedSession(t *testing.T) {
	f := &fakeSim{values: map[string]float64{Started: 1, Speed: 12}, failGet: 1}
	s := New(f, "")
	var clock time.Duration
	s.Sleep = func(ctx context.Context, d time.Duration) error { clock += d; return ctx.Err() }
	// Mix the panel's HTML and plain-text formats while walking the service and operator menus.
	menus := []Menu{
		ParseMenu("<b>Ramp Services</b><br>[1] Additional Services<br>2. Close"),
		ParseMenu("Additional Services\r\n# ignored\r\n1: Request potable water\r\n2) Back"),
		ParseMenu("Choose your operator\n1. GSX Choice\n2. Swissport"),
	}
	page := 0
	s.ReadMenu = func() Menu { return menus[min(page, len(menus)-1)] }
	f.onWrite = func(key string, value float64) {
		if key == MenuChoice {
			page++
		}
	}
	if ready, speed, err := s.Status(context.Background()); !ready || speed != 12 || err != nil {
		t.Fatalf("status = %v, %v, %v", ready, speed, err)
	}
	selected, err := s.Execute(context.Background(), "potable water", "", nil, true)
	if err != nil || selected != "Request potable water" {
		t.Fatalf("selection = %q, %v", selected, err)
	}
	if err := s.Close(); err != nil || !f.closed || s.Connected() {
		t.Fatalf("close = %v, %+v", err, f)
	}
	want := []operation{{MenuChoice, 0}, {MenuChoice, 0}, {MenuChoice, 0}, {MenuOpen, 0}, {Remote, 0}}
	if !reflect.DeepEqual(f.writes, want) {
		t.Errorf("writes = %+v, want %+v", f.writes, want)
	}
	if clock != 2080*time.Millisecond {
		t.Errorf("clock = %v", clock)
	}
	s.Sim = &fakeSim{values: map[string]float64{Started: 1}}
	if ready, _, err := s.Status(context.Background()); !ready || err != nil {
		t.Fatalf("reconnect = %v, %v", ready, err)
	}
}

func TestHotfixMenu(t *testing.T) {
	s := New(&fakeSim{values: map[string]float64{}}, "")
	// Use the navigation-only menu that GSX reports when the hotfix is needed.
	s.ReadMenu = func() Menu { return ParseMenu("GSX\nBack\nCancel") }
	if _, err := s.Execute(context.Background(), "request boarding", "", nil, false); !errors.Is(err, ErrHotfix) {
		t.Fatal(err)
	}
}

func TestClosePreservesUnownedMenu(t *testing.T) {
	// A menu can be open without this session owning remote control.
	f := &fakeSim{values: map[string]float64{MenuOpen: 1, Remote: 1}}
	s := New(f, "")
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if err := s.Close(); err != nil || !f.closed || s.Connected() {
		t.Fatalf("closing a monitoring session: %v", err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("closing a monitoring session wrote simulator variables: %v", f.writes)
	}
}

func TestChooseRejectsOutOfRangeMenuItems(t *testing.T) {
	f := &fakeSim{values: map[string]float64{}}
	s := New(f, "")
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	// Check both ends of the GSX menu's zero-based slot range.
	for _, index := range []int{-1, 9} {
		if err := s.Choose(t.Context(), index); err == nil {
			t.Errorf("accepted out-of-range index %d", index)
		}
	}
	if err := s.Choose(t.Context(), 8); err != nil {
		t.Fatal(err)
	}
	if want := []operation{{MenuChoice, 8}}; !reflect.DeepEqual(f.writes, want) {
		t.Errorf("writes = %v, want %v", f.writes, want)
	}
}

func TestRemoteOpenSettlesBeforeSelection(t *testing.T) {
	f := &fakeSim{values: map[string]float64{}}
	s := New(f, "")
	var clock time.Duration
	s.Sleep = func(ctx context.Context, d time.Duration) error { clock += d; return ctx.Err() }
	if err := s.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	if clock != 500*time.Millisecond || !s.RemoteActive() {
		t.Fatalf("open clock=%s remote=%v", clock, s.RemoteActive())
	}
	// Check the whole flag sequence because each write depends on the previous one.
	want := []operation{{Remote, 1}, {MenuOpen, 1}, {Remote, 0}, {Remote, 1}}
	if !reflect.DeepEqual(f.writes, want) {
		t.Fatalf("open writes=%v", f.writes)
	}
}

func TestInterruptedOpenCanRelease(t *testing.T) {
	f := &fakeSim{values: map[string]float64{}}
	s := New(f, "")
	ctx, cancel := context.WithCancel(t.Context())
	// Cancel in the settle delay after remote control has already been requested.
	s.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	if err := s.Open(ctx); !errors.Is(err, context.Canceled) || !s.RemoteActive() || f.values[Remote] != 1 {
		t.Fatalf("interrupted open: error %v, remote %v, variables %v", err, s.RemoteActive(), f.values)
	}
	if err := s.Release(ctx); err == nil || !s.RemoteActive() {
		t.Fatalf("failed release: error %v, remote %v", err, s.RemoteActive())
	}
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if err := s.Release(t.Context()); err != nil || s.RemoteActive() || f.values[Remote] != 0 {
		t.Fatalf("retried release: error %v, remote %v, variables %v", err, s.RemoteActive(), f.values)
	}
}

func TestServiceUsesCustomMenuKeywords(t *testing.T) {
	f := &fakeSim{values: map[string]float64{}}
	s := New(f, "")
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	// The custom phrase must win even when a built-in alias matches an earlier item.
	s.ReadMenu = func() Menu { return ParseMenu("GSX\n1. Jetway status\n2. Operate boarding bridge") }
	selected, err := s.Execute(t.Context(), "bridge", "JETWAY", []string{"boarding bridge"}, false)
	if err != nil || selected != "Operate boarding bridge" || f.values[MenuChoice] != 1 {
		t.Fatalf("selected %q, choice %v, error %v", selected, f.values[MenuChoice], err)
	}
}

func TestExecuteWaitsForAdditionalServices(t *testing.T) {
	f := &fakeSim{values: map[string]float64{}}
	s := New(f, "")
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	parent := ParseMenu("Ramp\nAdditional Services\nClose")
	child := ParseMenu("Additional Services\nRequest potable water\nBack")
	opened, stale := false, true
	f.onWrite = func(key string, value float64) {
		if key == MenuChoice && value == 0 {
			opened = true
		}
	}
	s.ReadMenu = func() Menu {
		if !opened {
			return parent
		}
		// The first read after selection still sees the old panel file.
		if stale {
			stale = false
			return parent
		}
		return child
	}
	selected, err := s.Execute(t.Context(), "potable water", "", nil, false)
	if want := []operation{{MenuChoice, 0}, {MenuChoice, 0}}; err != nil || selected != "Request potable water" || !reflect.DeepEqual(f.writes, want) {
		t.Fatalf("selection %q, error %v, writes %v, want %v", selected, err, f.writes, want)
	}
}

func TestStatusRequiresGroundSpeed(t *testing.T) {
	// A failed speed query and a dropped simulator connection should report different errors.
	s := New(&fakeSim{values: map[string]float64{Started: 1}, missing: Speed}, "")
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	for range 3 {
		if ready, _, err := s.Status(t.Context()); !ready || !errors.Is(err, ErrSpeedUnavailable) {
			t.Fatalf("speed read failed on a ready connection: ready %v, error %v", ready, err)
		}
	}
	s.Sim = nil
	if ready, _, err := s.Status(t.Context()); ready || !errors.Is(err, ErrDisconnected) || errors.Is(err, ErrSpeedUnavailable) {
		t.Fatalf("unexpected disconnected status: ready %v, error %v", ready, err)
	}
}

func TestCanceledOpenDoesNotClaimExistingMenu(t *testing.T) {
	// Cancel before taking over a menu that belonged to another client.
	f := &fakeSim{values: map[string]float64{MenuOpen: 1}}
	s := New(f, "")
	ctx, cancel := context.WithCancel(t.Context())
	s.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	if err := s.Open(ctx); !errors.Is(err, context.Canceled) || s.RemoteActive() || f.values[MenuOpen] != 1 {
		t.Fatalf("menu ownership changed: err %v, remote %v, menu %v", err, s.RemoteActive(), f.values[MenuOpen])
	}
}

func TestStatusRejectsNonFiniteGroundSpeed(t *testing.T) {
	f := &fakeSim{values: map[string]float64{Started: 1}}
	s := New(f, "")
	// All nonfinite values must fail the speed check even when Couatl is ready.
	for _, speed := range []float64{math.NaN(), math.Inf(-1), math.Inf(1)} {
		f.values[Speed] = speed
		ready, got, err := s.Status(t.Context())
		if !ready || got != 0 || !errors.Is(err, ErrSpeedUnavailable) {
			t.Errorf("speed %v: ready=%v speed=%v err=%v", speed, ready, got, err)
		}
	}
}
