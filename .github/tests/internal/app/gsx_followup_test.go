// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/gsx"
)

// Apply the navigation choice before the helper reads the momentary slot.
type consumedNavigationTransport struct {
	menuChoices
	page gsx.Menu
}

func (f *consumedNavigationTransport) Set(ctx context.Context, key string, value float64) error {
	if err := f.menuChoices.Set(ctx, key, value); err != nil {
		return err
	}
	if key == gsx.MenuChoice && len(f.selected) == 1 {
		f.page = gsx.ParseMenu("Another menu\nBack\nRequest stairs")
		return gsx.ErrWriteUncertain
	}
	return nil
}

func TestConsumedNavigationStillFindsService(t *testing.T) {
	ctx := t.Context()
	s := &state{dir: t.TempDir(), cfg: config.Defaults(), ctx: ctx, speechReady: true,
		services: make(map[string]bool), jobs: make(chan func(context.Context, *gsx.Session), 1)}
	s.cfg.Put("simulation_mode", true)
	s.cfg.Put("auto_select_operator", false)
	s.cfg.Put("mute_gsx_audio_during_crew_voice", false)
	w := gui.NewTestWindow(t, gui.WindowCfg{State: s, OnInit: func(w *gui.Window) { w.SetView(mainView) }})
	s.main = w
	transport := &consumedNavigationTransport{page: gsx.ParseMenu("Ramp\nAdditional Services")}
	session := gsx.New(transport, s.dir)
	session.ReadMenu = func() gsx.Menu { return transport.page }
	polls := 0
	session.Sleep = func(ctx context.Context, delay time.Duration) error {
		if delay == 500*time.Millisecond && len(transport.selected) == 1 {
			polls++
			if polls == 2 {
				transport.page = gsx.ParseMenu("Additional Services\nBack\nRequest stairs")
			}
		}
		return ctx.Err()
	}
	s.submit("request stairs")
	(<-s.jobs)(ctx, session)
	w.FrameFn()
	if !slices.Equal(transport.selected, []int{0, 1}) {
		t.Fatalf("submenu did open, but wrote slots %v; log %v", transport.selected, s.lines)
	}
}

func TestLostReplyStillStartsAudioProtection(t *testing.T) {
	page := gsx.ParseMenu("Ramp\nOperate jetway")
	lostReply := errors.Join(gsx.ErrWriteUncertain, gsx.ErrDisconnected)
	transport := &menuChoices{choiceError: func(int) error { return lostReply }}
	session := gsx.New(transport, t.TempDir())
	session.ReadMenu = func() gsx.Menu { return page }
	protected := false
	session.OnSelectionSent = func() { protected = true }
	err := session.ChooseOn(t.Context(), page, 0)
	if !errors.Is(err, gsx.ErrWriteUncertain) || !protected {
		t.Fatalf("possibly applied choice with lost reply left protected=%t; err=%v", protected, err)
	}
}

// Scope cancellation must not cancel transport exchanges or skip cleanup.
func TestCancelReadoutDuringOpenWrite(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	s := &state{dir: dir, cfg: config.Defaults(), ctx: ctx, jobs: make(chan func(context.Context, *gsx.Session), 1)}
	s.cfg.Put(config.MenuReadoutSetting, true)
	s.cfg.Put("gsx_menu_reader_keyword", "read menu")
	s.readouts = newReadoutScope(ctx)
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	stubKokoroModel(t, dir)
	transport := &cancelReadoutTransport{cancel: s.readouts.cancel}
	session := gsx.New(transport, dir)
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	s.submit("read menu")
	(<-s.jobs)(ctx, session)
	s.main.FrameFn()
	if !transport.opened || transport.canceledWrite || !transport.released || session.RemoteActive() ||
		strings.Contains(strings.Join(s.lines, "\n"), "context canceled") {
		t.Fatalf("opened=%t, canceled transport=%t, released=%t, remote=%t, log=%v",
			transport.opened, transport.canceledWrite, transport.released, session.RemoteActive(), s.lines)
	}
}

type cancelReadoutTransport struct {
	menuChoices
	cancel                          func()
	opened, canceledWrite, released bool
}

func (f *cancelReadoutTransport) Set(ctx context.Context, key string, value float64) error {
	switch {
	case key == gsx.MenuOpen && value == 1:
		f.opened = true
		f.cancel()
		f.canceledWrite = ctx.Err() != nil
	case key == gsx.MenuOpen && value == 0:
		f.released = true
	}
	return f.menuChoices.Set(ctx, key, value)
}

// Recover the panel writer after idle refresh has released ownership.
func TestOverlayRecoversAfterIdleRefreshWithoutLock(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	s := &state{dir: dir, cfg: config.Defaults(), ctx: ctx, ready: true, connected: true, done: make(chan struct{}), jobs: make(chan func(context.Context, *gsx.Session), 2)}
	w := gui.NewTestWindow(t, gui.WindowCfg{State: s, Width: 380, Height: 560, OnInit: func(w *gui.Window) { w.SetView(mainView) }})
	s.main = w
	w.FrameFn()
	transport := &recoveringPanelTransport{path: filepath.Join(dir, "menu")}
	go s.simulator()
	defer func() { cancel(); <-s.done }()
	s.jobs <- func(ctx context.Context, session *gsx.Session) {
		*session = *gsx.New(transport, dir)
		session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	}
	if err := w.TestClick("gsx-menu"); err != nil {
		t.Fatal(err)
	}
	s.jobs <- func(_ context.Context, _ *gsx.Session) { s.menuActivity = time.Now().Add(-time.Minute) }
	deadline := time.Now().Add(5 * time.Second)
	for transport.closes.Load() < 2 && time.Now().Before(deadline) {
		w.FrameFn()
		time.Sleep(10 * time.Millisecond)
	}
	if transport.closes.Load() < 2 {
		t.Fatal("fixture did not finish the idle recovery attempt")
	}
	previous := transport.opens.Load()
	transport.ready.Store(true)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w.FrameFn()
		if len(s.menu.Options) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("overlay stayed empty after recovery, open requests %d before and %d after", previous, transport.opens.Load())
}

type recoveringPanelTransport struct {
	stalledPanelTransport
	path  string
	ready atomic.Bool
	opens atomic.Int32
}

// A publication during cooldown is enough to recover the overlay
func TestOverlayAcceptsLatePublicationWithoutRepulsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "menu")
	s := &state{ctx: t.Context(), menuWatch: true, overlay: true}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	transport := &recoveringPanelTransport{path: path}
	session := gsx.New(transport, dir)
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if err := s.openOverlayMenu(s.ctx, session); !errors.Is(err, gsx.ErrMenuInactive) {
		t.Fatalf("expected the panel to miss its first deadline, got %v", err)
	}
	opens := transport.opens.Load()
	if err := os.WriteFile(path, []byte("Late ramp\nStairs"), 0600); err != nil {
		t.Fatal(err)
	}
	s.retryOverlayMenu(session)
	s.main.FrameFn()
	if s.menu.Title != "Late ramp" || transport.opens.Load() != opens || !s.menuRetryAt.IsZero() {
		t.Fatalf("late menu=%+v, opens=%d, retry=%v", s.menu, transport.opens.Load(), s.menuRetryAt)
	}
}

func (f *recoveringPanelTransport) Set(ctx context.Context, key string, value float64) error {
	if key == gsx.MenuOpen && value == 1 {
		f.opens.Add(1)
		if f.ready.Load() {
			return os.WriteFile(f.path, []byte("Recovered ramp\nStairs"), 0600)
		}
	}
	return f.stalledPanelTransport.Set(ctx, key, value)
}

func TestReadoutLeavesPilotMenuOpen(t *testing.T) {
	dir := t.TempDir()
	s := &state{dir: dir, cfg: config.Defaults(), ctx: t.Context(), speechReady: true, jobs: make(chan func(context.Context, *gsx.Session), 1)}
	s.cfg.Put("simulation_mode", true)
	s.cfg.Put(config.MenuReadoutSetting, true)
	s.cfg.Put("presets", []config.Preset{{Name: "Read ramp", Codeword: "read ramp", Enabled: true, Services: []string{config.ReadMenuAction}}})
	s.readouts = newReadoutScope(s.ctx)
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	stubKokoroModel(t, dir)
	transport := &pilotMenuTransport{menuChoices: menuChoices{open: true}}
	session := gsx.New(transport, dir)
	var pilotPage gsx.Menu
	session.ReadMenu = func() gsx.Menu { return pilotPage }
	session.Sleep = func(ctx context.Context, delay time.Duration) error {
		// The pilot already opened the menu, and Couatl publishes the rows later
		if delay == 500*time.Millisecond {
			pilotPage = gsx.ParseMenu("Pilot ramp\nStairs")
		}
		return ctx.Err()
	}
	s.submit("read ramp")
	(<-s.jobs)(s.ctx, session)
	s.main.FrameFn()
	if len(s.audioQueue) != 1 {
		t.Fatalf("readout did not queue: %v", s.lines)
	}
	if !transport.open || transport.writes != 0 {
		t.Fatalf("readout changed pilot menu, open=%t, simulator writes=%d", transport.open, transport.writes)
	}
}

type pilotMenuTransport struct {
	menuChoices
	writes int
}

func (f *pilotMenuTransport) Get(ctx context.Context, key string) (float64, error) {
	if key == gsx.MenuOpen && f.open {
		return 1, ctx.Err()
	}
	return 0, ctx.Err()
}
func (f *pilotMenuTransport) Set(ctx context.Context, key string, value float64) error {
	f.writes++
	return f.menuChoices.Set(ctx, key, value)
}
func TestManualOperatorStillAcknowledgesService(t *testing.T) {
	dir := t.TempDir()
	s := &state{dir: dir, cfg: config.Defaults(), ctx: t.Context(), speechReady: true,
		jobs: make(chan func(context.Context, *gsx.Session), 2), services: make(map[string]bool)}
	s.cfg.Put("auto_select_operator", false)
	s.cfg.Put("mute_gsx_audio_during_crew_voice", false)
	s.groundSpeed.Store(&speedReading{0, time.Now()})
	w := gui.NewTestWindow(t, gui.WindowCfg{State: s, Width: 380, Height: 560})
	s.main = w
	transport := &consumedChoiceTransport{page: gsx.ParseMenu("Ramp\nOperate jetway")}
	session := gsx.New(transport, dir)
	session.ReadMenu = func() gsx.Menu { return transport.page }
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	s.submit("request jetway")
	(<-s.jobs)(s.ctx, session)
	w.FrameFn()
	if transport.count != 1 {
		t.Fatalf("service applied and published operator page, but app issued %d choice(s); log=%v", transport.count, s.lines)
	}
	if !strings.Contains(strings.Join(s.lines, "\n"), "matched and selected") {
		t.Fatalf("confirmed service was not acknowledged: %v", s.lines)
	}
	if !s.overlay || !s.menu.Same(transport.page) {
		t.Fatal("pilot cannot reach the manual operator menu")
	}
	s.submit("one")
	s.submit("one")
	(<-s.jobs)(s.ctx, session)
	(<-s.jobs)(s.ctx, session)
	if transport.count != 2 || s.operatorSubmitted.IsZero() {
		t.Fatalf("manual selection left choices=%d, operator=%+v", transport.count, s.operatorMenu)
	}
}
