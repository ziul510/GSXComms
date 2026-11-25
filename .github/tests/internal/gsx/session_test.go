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
	"testing/synctest"
	"time"

	"golang.org/x/sys/windows"
)

type operation struct {
	key   string
	value float64
}
type fakeSim struct {
	values          map[string]float64
	writes          []operation
	closed          bool
	onWrite         func(string, float64)
	onRead          func(string)
	failGet         int
	missing         string
	uncertainRemote bool
}

func (f *fakeSim) Get(ctx context.Context, key string) (float64, error) {
	if f.onRead != nil {
		f.onRead(key)
	}
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
	// The simulator may apply a write even when readback cannot verify it.
	if f.uncertainRemote && key == Remote && value == 1 {
		return ErrWriteUncertain
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
	session.ReadMenu = func() Menu {
		// An independent panel update cannot verify an uncertain selection.
		if transport.attempts > 0 {
			return ParseMenu("Another menu\nStairs")
		}
		return page
	}
	// Start audio protection and settle without highlighting an unconfirmed selection.
	protected, settled := false, false
	session.OnChoice = func(Menu, int) { t.Error("uncertain write was highlighted") }
	session.OnSelectionSent = func() { protected = true }
	session.Sleep = func(_ context.Context, d time.Duration) error {
		settled = d == 300*time.Millisecond
		return nil
	}
	if err := session.ChooseOn(t.Context(), page, 0); !errors.Is(err, ErrWriteUncertain) || transport.attempts != 1 {
		t.Fatalf("choice error = %v, attempts = %d", err, transport.attempts)
	}
	if !protected || !settled {
		t.Fatalf("uncertain choice left protected=%t, settled=%t", protected, settled)
	}
}

func TestUncertainRemoteEnableCanRelease(t *testing.T) {
	for _, open := range []float64{0, 1} {
		f := &fakeSim{values: map[string]float64{MenuOpen: open}, uncertainRemote: true}
		s := New(f, t.TempDir())
		s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
		if err := s.Open(t.Context()); !errors.Is(err, ErrWriteUncertain) || !s.RemoteActive() {
			t.Fatalf("menu flag = %g, open error = %v, remote tracked = %t", open, err, s.RemoteActive())
		}
		if err := s.Close(); err != nil || f.values[Remote] != 0 || !f.closed {
			t.Fatalf("menu flag = %g, cleanup error = %v, simulator = %+v", open, err, f)
		}
	}
}

func TestOpenRejectsUnreadablePreexistingPanel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "menu")
	if err := os.WriteFile(path, []byte("Old ramp\nJetway"), 0600); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate Couatl holding a stale file exclusively during the pre-open read.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	f := &fakeSim{values: map[string]float64{}}
	s := New(f, dir)
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if _, err := s.OpenMenuPage(t.Context()); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || len(f.writes) != 0 {
		t.Fatalf("open error = %v, writes = %v", err, f.writes)
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
	s.OnSelectionSent = func() {
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
	// The operator page is already visible after the service choice settles.
	if clock != 2080*time.Millisecond {
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
	closeErr error
}

func (f *operatorSim) Set(ctx context.Context, key string, value float64) error {
	// Fail only the close write so the test still reaches the operator menu.
	if key == MenuOpen && value == 0 && f.closeErr != nil {
		return f.closeErr
	}
	return f.fakeSim.Set(ctx, key, value)
}

func TestOperatorWithoutGSXChoiceAttemptsClose(t *testing.T) {
	for _, closeErr := range []error{nil, errors.New("menu could not close")} {
		f := &operatorSim{fakeSim: &fakeSim{values: map[string]float64{}}, closeErr: closeErr}
		menu := ParseMenu("Ramp\nRequest Jetway\nClose")
		f.onWrite = func(key string, value float64) {
			if key == MenuChoice {
				menu = ParseMenu("Choose your operator\nSwissport\nMenzies")
			}
		}
		session := New(f, "")
		session.ReadMenu = func() Menu { return menu }
		session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
		selected, err := session.Execute(t.Context(), "", "JETWAY", []string{"jetway"}, true)
		if selected != "Request Jetway" || !errors.Is(err, closeErr) {
			t.Fatalf("close error = %v, selected = %q, want error %v and Request Jetway", err, selected, closeErr)
		}
		choices := 0
		for _, write := range f.writes {
			if write.key == MenuChoice {
				choices++
			}
		}
		// Auto mode sends only the service choice and releases the remote flag.
		if choices != 1 || f.values[Remote] != 0 || (f.values[MenuOpen] == 1) != (closeErr != nil) {
			t.Fatalf("close error = %v, open = %v, remote = %v, choices = %d", closeErr, f.values[MenuOpen], f.values[Remote], choices)
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

func TestReopenRequiresFreshPanelWhenAlreadyRemote(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "menu")
	if err := os.WriteFile(path, []byte("Ramp\nJetway"), 0600); err != nil {
		t.Fatal(err)
	}
	sim := &fakeSim{values: map[string]float64{MenuOpen: 1, Remote: 1}}
	s := New(sim, dir)
	s.remote = true
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	// A service requests a refresh even though the session already owns the menu.
	if _, err := s.Execute(t.Context(), "", "JETWAY", []string{"jetway"}, false); !errors.Is(err, ErrMenuInactive) {
		t.Fatalf("unchanged panel after reopening: %v", err)
	}
	for _, write := range sim.writes {
		if write.key == MenuChoice {
			t.Fatal("sent a choice from the previous panel")
		}
	}
}

// A lock before any write should not hide a later menu opened by the pilot
func TestReadoutReusesPilotMenuAfterAbandonedLockedOpen(t *testing.T) {
	sim := &fakeSim{values: make(map[string]float64)}
	s := New(sim, t.TempDir())
	s.ReadMenu = func() Menu { return Menu{readErr: windows.ERROR_SHARING_VIOLATION} }
	if err := s.Open(t.Context()); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("open error = %v", err)
	}
	sim.values[MenuOpen] = 1
	want := ParseMenu("Pilot ramp\nStairs")
	s.ReadMenu = func() Menu { return want }
	if page, err := s.ReadMenuPage(t.Context(), t.Context()); err != nil || !page.Same(want) || len(sim.writes) != 0 {
		t.Fatalf("pilot menu = %+v, err = %v, writes = %v", page, err, sim.writes)
	}
}

// Losing the readout's wait before it opens a menu should release only its flag
func TestReadoutCancelBeforeOpenPreservesPilotMenu(t *testing.T) {
	sim := &fakeSim{values: make(map[string]float64)}
	s := New(sim, t.TempDir())
	wait, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.Sleep = func(ctx context.Context, _ time.Duration) error {
		sim.values[MenuOpen] = 1
		cancel()
		return ctx.Err()
	}
	_, err := s.ReadMenuPage(t.Context(), wait)
	wantWrites := []operation{{Remote, 1}, {Remote, 0}}
	if !errors.Is(err, context.Canceled) || sim.values[MenuOpen] != 1 || !reflect.DeepEqual(sim.writes, wantWrites) {
		t.Fatalf("cancel: err=%v, open=%v, writes=%v", err, sim.values[MenuOpen], sim.writes)
	}
}

// A failed release must not change which simulator state shutdown can close
func TestReadoutRetriesOnlyRemoteFlagOnShutdown(t *testing.T) {
	sim := &uncertainFlagRelease{fakeSim: &fakeSim{values: make(map[string]float64)}}
	s := New(sim, t.TempDir())
	wait, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.Sleep = func(ctx context.Context, _ time.Duration) error {
		sim.values[MenuOpen] = 1
		cancel()
		return ctx.Err()
	}
	if _, err := s.ReadMenuPage(t.Context(), wait); !errors.Is(err, ErrWriteUncertain) {
		t.Fatalf("first release error = %v", err)
	}
	if err := s.Close(); err != nil || sim.values[MenuOpen] != 1 ||
		!reflect.DeepEqual(sim.writes, []operation{{Remote, 1}, {Remote, 0}}) {
		t.Fatalf("shutdown err=%v, writes=%v, values=%v", err, sim.writes, sim.values)
	}
}

// Fail once without confirming the remote flag was cleared
type uncertainFlagRelease struct {
	*fakeSim
	rejected       bool
	afterMenuClose bool
}

func (f *uncertainFlagRelease) Set(ctx context.Context, key string, value float64) error {
	if key == Remote && value == 0 && !f.rejected && (!f.afterMenuClose || f.values[MenuOpen] == 0) {
		f.rejected = true
		return ErrWriteUncertain
	}
	return f.fakeSim.Set(ctx, key, value)
}

// A completed close leaves no authority to close a later pilot menu
func TestReadoutAfterPartialReleaseLeavesNewPilotMenuOpen(t *testing.T) {
	sim := &uncertainFlagRelease{fakeSim: &fakeSim{values: make(map[string]float64)}, afterMenuClose: true}
	s := New(sim, t.TempDir())
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	s.ReadMenu = func() Menu {
		if sim.values[MenuOpen] == 1 {
			return ParseMenu("Ramp\nStairs")
		}
		return Menu{}
	}
	if _, err := s.ReadMenuPage(t.Context(), t.Context()); !errors.Is(err, ErrWriteUncertain) {
		t.Fatalf("first release error = %v", err)
	}
	sim.values[MenuOpen] = 1
	before := len(sim.writes)
	if _, err := s.RefreshAndRelease(t.Context()); err != nil || sim.values[MenuOpen] != 1 ||
		!reflect.DeepEqual(sim.writes[before:], []operation{{Remote, 0}}) {
		t.Fatalf("idle cleanup: err=%v, values=%v, writes=%v", err, sim.values, sim.writes[before:])
	}
}

// The transport health read can overlap a same-row rewrite before the pulse
func TestIdleReopenRejectsRewriteDuringMenuFlagRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "menu")
	if err := os.WriteFile(path, []byte("Ramp\nRequest stairs"), 0600); err != nil {
		t.Fatal(err)
	}
	sim := &fakeSim{values: make(map[string]float64)}
	s := New(sim, dir)
	shown := s.CurrentMenu()
	sim.onRead = func(key string) {
		if key == MenuOpen {
			rewritten := time.Now().Add(time.Minute)
			if err := os.Chtimes(path, rewritten, rewritten); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.ReopenMenuPage(t.Context(), shown); !errors.Is(err, ErrMenuChanged) || len(sim.writes) != 0 {
		t.Fatalf("reopen: err=%v, writes=%v", err, sim.writes)
	}
}

// A rewrite during remote acquisition is still earlier than the refresh write
func TestIdleReopenRejectsRewriteBeforeOpeningWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "menu")
	if err := os.WriteFile(path, []byte("Ramp\nRequest stairs"), 0600); err != nil {
		t.Fatal(err)
	}
	sim := &fakeSim{values: make(map[string]float64)}
	s := New(sim, dir)
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	shown := s.CurrentMenu()
	sim.onWrite = func(key string, value float64) {
		if key == Remote && value == 1 {
			rewritten := time.Now().Add(time.Minute)
			if err := os.Chtimes(path, rewritten, rewritten); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.ReopenMenuPage(t.Context(), shown); !errors.Is(err, ErrMenuChanged) ||
		!reflect.DeepEqual(sim.writes, []operation{{Remote, 1}, {Remote, 0}}) {
		t.Fatalf("reopen: err=%v, writes=%v", err, sim.writes)
	}
}

// A late first publication can arrive during the retry's simulator read
func TestOpenAcceptsFreshPanelDuringRetryRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "menu")
	sim := &fakeSim{values: make(map[string]float64)}
	s := New(sim, dir)
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	reads := 0
	sim.onRead = func(key string) {
		if key == MenuOpen {
			reads++
			if reads == 2 {
				if err := os.WriteFile(path, []byte("Late ramp\nRequest stairs"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if page, err := s.OpenMenuPage(t.Context()); err != nil || page.Title != "Late ramp" || len(sim.writes) != 4 {
		t.Fatalf("open: page=%+v, err=%v, writes=%v", page, err, sim.writes)
	}
}

// Recovery can wait for delayed pilot rows without taking remote control
func TestRetryLeavesUnownedOpenMenuAlone(t *testing.T) {
	sim := &fakeSim{values: map[string]float64{MenuOpen: 1}}
	s := New(sim, t.TempDir())
	var page Menu
	s.ReadMenu = func() Menu { return page }
	s.Sleep = func(ctx context.Context, _ time.Duration) error {
		page = ParseMenu("Pilot ramp\nStairs")
		return ctx.Err()
	}
	if got, err := s.RetryMenuPage(t.Context()); err != nil || got.Title != "Pilot ramp" || len(sim.writes) != 0 {
		t.Fatalf("pilot menu: got=%+v, err=%v, writes=%v", got, err, sim.writes)
	}
}

// Once recovery opened the menu, it can pulse it again if the first wait expires
func TestRetryRepulsesMenuItOpened(t *testing.T) {
	dir := t.TempDir()
	sim := &fakeSim{values: make(map[string]float64)}
	s := New(sim, dir)
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	enables := 0
	sim.onWrite = func(key string, value float64) {
		if key == Remote && value == 1 {
			enables++
			if enables == 3 {
				if err := os.WriteFile(filepath.Join(dir, "menu"), []byte("Recovered ramp\nStairs"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if got, err := s.RetryMenuPage(t.Context()); err != nil || got.Title != "Recovered ramp" || enables != 3 {
		t.Fatalf("recovery: got=%+v, err=%v, remote enables=%d", got, err, enables)
	}
}

func TestExecuteReportsConfirmedChoiceWhenSettleFails(t *testing.T) {
	sim := &fakeSim{values: map[string]float64{MenuOpen: 1, Remote: 1}}
	s := New(sim, t.TempDir())
	s.remote = true
	s.ReadMenu = func() Menu { return ParseMenu("Ramp\nJetway") }
	s.Sleep = func(ctx context.Context, delay time.Duration) error {
		if delay == 300*time.Millisecond {
			return context.DeadlineExceeded
		}
		return ctx.Err()
	}
	selected, err := s.Execute(t.Context(), "", "JETWAY", []string{"jetway"}, false)
	if selected != "Jetway" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("confirmed choice lost after settle failure: %q, %v", selected, err)
	}
}

type delayedReleaseSim struct {
	*fakeSim
	rejected bool
}

func (f *delayedReleaseSim) Set(ctx context.Context, key string, value float64) error {
	// Model a definite rejection followed by full confirmation windows.
	if key == MenuOpen && !f.rejected {
		f.rejected = true
		if err := wait(ctx, 250*time.Millisecond); err != nil {
			return err
		}
		return errors.New("menu close rejected")
	}
	if err := wait(ctx, 350*time.Millisecond); err != nil {
		return err
	}
	return f.fakeSim.Set(ctx, key, value)
}

func TestCloseAllowsReleaseRetryWithFullWriteWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &delayedReleaseSim{fakeSim: &fakeSim{values: map[string]float64{MenuOpen: 1, Remote: 1}}}
		s := New(f, t.TempDir())
		s.remote = true
		if err := s.Close(); err != nil || f.values[MenuOpen] != 0 || f.values[Remote] != 0 || !f.closed {
			t.Fatalf("release did not finish after a retry: %v, values=%v, closed=%v", err, f.values, f.closed)
		}
	})
}
