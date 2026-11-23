// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
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
	// Simulate a single unavailable variable without taking the whole transport offline
	if key == f.missing {
		return 0, errors.New("unavailable simulator variable")
	}
	// Inject transient reads so the script also exercises the retry loop
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
	// Record every flag and choice so tests can verify protocol order
	f.writes = append(f.writes, operation{key, value})
	// A choice can advance the fake panel like the real simulator would
	if f.onWrite != nil {
		f.onWrite(key, value)
	}
	return nil
}
func (f *fakeSim) Close() error { f.closed = true; return nil } // Track whether cleanup actually closed the transport

type uncertainWrite struct {
	*fakeSim
	attempts int
}

func (f *uncertainWrite) Set(context.Context, string, float64) error {
	f.attempts++
	return ErrWriteUncertain
}

func TestSelectionDoesNotReplayAnUncertainWrite(t *testing.T) {
	transport := &uncertainWrite{fakeSim: &fakeSim{}}
	session := New(transport, t.TempDir())
	page := ParseMenu("Ramp\nJetway")
	session.ReadMenu = func() Menu { return page }
	// A lost reply leaves the selection unconfirmed, so the UI should get no callback.
	session.OnChoice = func(Menu, int) { t.Error("uncertain write was highlighted") }
	session.AfterSelection = func() { t.Error("uncertain write triggered selection callback") }
	if err := session.ChooseOn(t.Context(), page, 0); !errors.Is(err, ErrWriteUncertain) || transport.attempts != 1 {
		t.Fatalf("choice error = %v, attempts = %d", err, transport.attempts)
	}
}

func TestScriptedSession(t *testing.T) {
	f := &fakeSim{values: map[string]float64{Started: 1, Speed: 12}, failGet: 1}
	s := New(f, "")
	var clock time.Duration
	// Count the required GSX delays without making the test wait for them.
	s.Sleep = func(ctx context.Context, d time.Duration) error { clock += d; return ctx.Err() }
	// Mix the panel's HTML and plain text formats while walking the service and operator menus
	menus := []Menu{
		ParseMenu("<b>Ramp Services</b><br>[1] Additional Services<br>2. Close"),
		ParseMenu("Additional Services\r\n# ignored\r\n1: Request potable water\r\n2) Back"),
		ParseMenu("Choose your operator\n1. GSX Choice\n2. Swissport"),
	}
	page := 0
	s.ReadMenu = func() Menu { return menus[min(page, len(menus)-1)] }
	var choiceAt time.Duration
	f.onWrite = func(key string, value float64) {
		if key == MenuChoice {
			page++
			choiceAt = clock
		}
	}
	selections := 0
	// Notify after writing the service or operator choice, before the panel delay.
	s.AfterSelection = func() {
		selections++
		if page < 2 || clock != choiceAt {
			t.Errorf("selection callback on page %d at %v, last write at %v", page, clock, choiceAt)
		}
	}
	if ready, speed, err := s.Status(context.Background()); !ready || speed != 12 || err != nil {
		t.Fatalf("status = %v, %v, %v", ready, speed, err)
	}
	selected, err := s.Execute(context.Background(), "potable water", "", nil, true)
	if err != nil || selected != "Request potable water" {
		t.Fatalf("selection = %q, %v", selected, err)
	}
	if selections != 2 {
		t.Errorf("got %d selection callbacks, want service and operator", selections)
	}
	if err := s.Close(); err != nil || !f.closed || s.Connected() {
		t.Fatalf("close = %v, %+v", err, f)
	}
	want := []operation{{Remote, 1}, {MenuOpen, 1}, {Remote, 0}, {Remote, 1}, {MenuChoice, 0}, {MenuChoice, 0}, {MenuChoice, 0}, {MenuOpen, 0}, {Remote, 0}}
	if !reflect.DeepEqual(f.writes, want) {
		t.Errorf("writes = %+v, want %+v", f.writes, want)
	}
	if clock != 2580*time.Millisecond {
		t.Errorf("clock = %v", clock)
	}
	s.Sim = &fakeSim{values: map[string]float64{Started: 1}}
	if ready, _, err := s.Status(context.Background()); !ready || err != nil {
		t.Fatalf("reconnect = %v, %v", ready, err)
	}
}

func TestHotfixMenu(t *testing.T) {
	for _, test := range []struct {
		options string
		hotfix  bool
	}{
		{"Back!\nClose.", true},
		{"Back\nCancel", true},
		{"Close\nCancel…", true},
		// Two spellings of Back alone should not look like the broken navigation-only menu.
		{"Back\nBack!", false},
		{"Back\nRequest boarding", false},
	} {
		t.Run(test.options, func(t *testing.T) {
			s := New(&fakeSim{values: map[string]float64{}}, "")
			s.ReadMenu = func() Menu { return ParseMenu("GSX\n" + test.options) }
			s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
			_, err := s.Execute(t.Context(), "request boarding", "", nil, false)
			if errors.Is(err, ErrHotfix) != test.hotfix {
				t.Fatalf("hotfix = %t, got %v", test.hotfix, err)
			}
		})
	}
}

func TestClosePreservesUnownedMenu(t *testing.T) {
	// A menu can be open without this session owning remote control
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
	// Check both GSX slot boundaries
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
	// Check the whole flag sequence because each write depends on the previous one
	want := []operation{{Remote, 1}, {MenuOpen, 1}, {Remote, 0}, {Remote, 1}}
	if !reflect.DeepEqual(f.writes, want) {
		t.Fatalf("open writes=%v", f.writes)
	}
}

func TestInterruptedOpenCanRelease(t *testing.T) {
	f := &fakeSim{values: map[string]float64{}}
	s := New(f, "")
	ctx, cancel := context.WithCancel(t.Context())
	// Cancel in the settle delay after remote control has already been requested
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
	// The custom phrase must win even when a built in alias matches an earlier item
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
		// The first read after selection still sees the old panel file
		if stale {
			stale = false
			return parent
		}
		return child
	}
	selected, err := s.Execute(t.Context(), "potable water", "", nil, false)
	if want := []operation{{Remote, 1}, {MenuOpen, 1}, {Remote, 0}, {Remote, 1}, {MenuChoice, 0}, {MenuChoice, 0}}; err != nil || selected != "Request potable water" || !reflect.DeepEqual(f.writes, want) {
		t.Fatalf("selection %q, error %v, writes %v, want %v", selected, err, f.writes, want)
	}
}

func TestMenuKeepsLabelsThatStartWithNumbers(t *testing.T) {
	// Mix menu prefixes with measurements and ranges so numeric label content stays intact.
	page := ParseMenu("Fuel\n10,000 lbs\n180\n[3] Close\n4.Back\n5)GSX Choice\n2.5 NM\n1-2")
	want := []string{"10,000 lbs", "180", "Close", "Back", "GSX Choice", "2.5 NM", "1-2"}
	if !reflect.DeepEqual(page.Options, want) {
		t.Fatalf("options = %v, want %v", page.Options, want)
	}
}

func TestCustomServicePrefersExactLabel(t *testing.T) {
	s := New(&fakeSim{values: map[string]float64{}}, "")
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	// Put the partial match first to test that the exact label wins over menu order.
	s.ReadMenu = func() Menu { return ParseMenu("Ramp\nRemove Chocks and Cones\nRemove Chocks") }
	selected, err := s.Execute(t.Context(), "", "Custom: Remove Chocks", []string{"Remove Chocks"}, false)
	if err != nil || selected != "Remove Chocks" {
		t.Fatalf("selected %q, error %v", selected, err)
	}
}

func TestStatusRequiresGroundSpeed(t *testing.T) {
	// A failed speed query and a dropped simulator connection should report different errors
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
	// Cancel before taking over a menu that belonged to another client
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
	// All nonfinite values must fail the speed check even when Couatl is ready
	for _, speed := range []float64{math.NaN(), math.Inf(-1), math.Inf(1)} {
		f.values[Speed] = speed
		ready, got, err := s.Status(t.Context())
		if !ready || got != 0 || !errors.Is(err, ErrSpeedUnavailable) {
			t.Errorf("speed %v: ready=%v speed=%v err=%v", speed, ready, got, err)
		}
	}
}

type operatorSim struct {
	*fakeSim
	failClose bool
}

func (f *operatorSim) Set(ctx context.Context, key string, value float64) error {
	// Fail only the close write so the test still reaches the operator menu.
	if key == MenuOpen && value == 0 && f.failClose {
		return errors.New("menu could not close")
	}
	return f.fakeSim.Set(ctx, key, value)
}

func TestOperatorWithoutGSXChoiceClosesBeforeFallback(t *testing.T) {
	for _, failClose := range []bool{false, true} {
		f := &operatorSim{fakeSim: &fakeSim{values: map[string]float64{}}, failClose: failClose}
		menu := ParseMenu("Ramp\nRequest Jetway\nClose")
		f.onWrite = func(key string, value float64) {
			if key == MenuChoice {
				menu = ParseMenu("Choose your operator\nSwissport\nMenzies")
			}
		}
		session := New(f, "")
		session.ReadMenu = func() Menu { return menu }
		session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
		if _, err := session.Execute(t.Context(), "", "JETWAY", []string{"jetway"}, true); err != nil {
			t.Fatal(err)
		}
		choices := 0
		for _, write := range f.writes {
			if write.key == MenuChoice && write.value == 0 {
				choices++
			}
		}
		// Select the first operator only if the attempt to close that menu failed.
		if (choices == 2) != failClose || (f.values[MenuOpen] == 1) != failClose {
			t.Fatalf("failedClose=%t open=%v choices=%d", failClose, f.values[MenuOpen], choices)
		}
	}
}

func TestIdleRefreshSnapshotsBeforeRelease(t *testing.T) {
	f := &fakeSim{values: map[string]float64{MenuOpen: 1, Remote: 1}}
	s := New(f, "")
	s.remote = true
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	want := ParseMenu("Updated\nRequest stairs")
	s.ReadMenu = func() Menu {
		// Hide the fake panel after release so a snapshot taken too late fails the test.
		if f.values[MenuOpen] == 1 && f.values[Remote] == 1 {
			return want
		}
		return Menu{}
	}
	page, err := s.RefreshAndRelease(t.Context())
	if err != nil || !page.Same(want) || s.RemoteActive() || f.values[MenuOpen] != 0 {
		t.Fatalf("snapshot=%+v remote=%t error=%v", page, s.RemoteActive(), err)
	}
	if len(f.writes) < 4 || f.writes[0] != (operation{MenuOpen, 0}) ||
		f.writes[len(f.writes)-2] != (operation{MenuOpen, 0}) {
		t.Fatalf("expected close, reopen, then close: %+v", f.writes)
	}
}

func TestFreshOpenRejectsPreviousPanel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "menu")
	const panel = "Ramp\nJetway"
	if err := os.WriteFile(path, []byte(panel), 0600); err != nil {
		t.Fatal(err)
	}
	sim := &fakeSim{values: make(map[string]float64)}
	s := New(sim, dir)
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if _, err := s.Execute(t.Context(), "jetway", "JETWAY", []string{"jetway"}, false); !errors.Is(err, ErrMenuInactive) {
		t.Fatalf("unchanged panel: %v", err)
	}
	for _, write := range sim.writes {
		if write.key == MenuChoice {
			t.Fatal("sent a choice from the stale file")
		}
	}
	// Couatl can rewrite an identical menu for a different open request.
	rewritten := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, rewritten, rewritten); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WaitMenu(t.Context()); err != nil {
		t.Fatalf("identical but newer panel: %v", err)
	}
}
