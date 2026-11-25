// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/gsx"
	"github.com/ziul510/GSXCommsGo/internal/models"
	"github.com/ziul510/GSXCommsGo/internal/speech"
)

// Exercise toolbar events and real panel reads on the headless window.
func TestOverlayRejectsNewGenerationWithSameRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "menu")
	if err := os.WriteFile(path, []byte("Previous\nOld row"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &state{dir: dir, cfg: config.Defaults(), ctx: t.Context(), ready: true, speechReady: true, jobs: make(chan func(context.Context, *gsx.Session), 1)}
	s.groundSpeed.Store(&speedReading{0, time.Now()})
	w := gui.NewTestWindow(t, gui.WindowCfg{State: s, Width: 380, Height: 560, OnInit: func(w *gui.Window) { w.SetView(mainView) }})
	s.main = w
	w.FrameFn()
	transport := &overlayOpenTransport{onOpen: func() error { return os.WriteFile(path, []byte("Ramp\nRequest stairs"), 0600) }}
	session := gsx.New(transport, dir)
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if err := w.TestClick("gsx-menu"); err != nil {
		t.Fatal(err)
	}
	(<-s.jobs)(s.ctx, session)
	w.FrameFn()
	if !s.menu.Same(gsx.ParseMenu("Ramp\nRequest stairs")) {
		t.Fatalf("fixture did not open fresh ramp: %+v", s.menu)
	}
	// Identical rows do not identify the same physical panel generation.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	s.submit("one")
	(<-s.jobs)(s.ctx, session)
	w.FrameFn()
	if len(transport.selected) != 0 {
		t.Fatalf("new panel generation accepted old displayed row: writes=%v", transport.selected)
	}
}

// Run the actual polling loop while the simulated panel writer stops responding.
func TestOverlayFailedRefreshNeverDisplaysOldPanel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "menu")
	if err := os.WriteFile(path, []byte("Previous\nOld row"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	s := &state{dir: dir, cfg: config.Defaults(), ctx: ctx, connected: true, ready: true, done: make(chan struct{}), jobs: make(chan func(context.Context, *gsx.Session), 2)}
	w := gui.NewTestWindow(t, gui.WindowCfg{State: s, Width: 380, Height: 560, OnInit: func(w *gui.Window) { w.SetView(mainView) }})
	s.main = w
	w.FrameFn()
	simul := &stalledPanelTransport{}
	go s.simulator()
	defer func() { cancel(); <-s.done }()
	s.jobs <- func(ctx context.Context, session *gsx.Session) {
		*session = *gsx.New(simul, dir)
		session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	}
	if err := w.TestClick("gsx-menu"); err != nil {
		t.Fatal(err)
	}
	missed := make(chan bool, 1)
	s.jobs <- func(_ context.Context, session *gsx.Session) {
		missed <- len(session.CurrentMenu().Options) == 0
		s.menuActivity = time.Now().Add(-time.Minute)
	}
	if !<-missed {
		t.Fatal("initial open failed to hide unchanged panel")
	}
	cutoff := time.Now().Add(4 * time.Second)
	for time.Now().Before(cutoff) {
		w.FrameFn()
		for _, command := range w.Renderers() {
			if command.Kind == gui.RenderText && strings.Contains(command.Text, "Old row") {
				t.Fatalf("headless window rendered stale row after failed refresh and %d close writes", simul.closes.Load())
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if simul.closes.Load() < 2 {
		t.Fatal("fixture did not exercise idle refresh and release")
	}
}

type stalledPanelTransport struct {
	closes atomic.Int32
}

func (*stalledPanelTransport) Get(ctx context.Context, key string) (float64, error) {
	if key == gsx.Started {
		return 1, ctx.Err()
	}
	return 0, ctx.Err()
}
func (a *stalledPanelTransport) Set(ctx context.Context, key string, value float64) error {
	if key == gsx.MenuOpen && value == 0 {
		a.closes.Add(1)
	}
	return ctx.Err()
}
func (*stalledPanelTransport) Close() error { return nil }

type consumedChoiceTransport struct {
	count int
	page  gsx.Menu
	open  bool
}

func (a *consumedChoiceTransport) Get(ctx context.Context, key string) (float64, error) {
	// The open flag can change independently of the panel the test published
	if key == gsx.MenuOpen && a.open {
		return 1, ctx.Err()
	}
	return 0, ctx.Err()
}
func (a *consumedChoiceTransport) Set(ctx context.Context, key string, value float64) error {
	if key == gsx.MenuOpen {
		a.open = value == 1
	}
	if key == gsx.MenuChoice {
		a.count++
		if a.count == 1 {
			// The service applied before the helper's readback, and opened an operator screen.
			a.page = gsx.ParseMenu("Select operator\nGSX choice")
			return gsx.ErrWriteUncertain
		}
	}
	return ctx.Err()
}
func (*consumedChoiceTransport) Close() error { return nil }

func TestConsumedChoiceStillRunsOperator(t *testing.T) {
	dir := t.TempDir()
	s := &state{dir: dir, cfg: config.Defaults(), ctx: t.Context(), speechReady: true,
		jobs: make(chan func(context.Context, *gsx.Session), 1), services: make(map[string]bool)}
	s.cfg.Put("auto_select_operator", true)
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
	if transport.count != 2 {
		t.Fatalf("service applied and published operator page, but app issued %d choice(s); log=%v", transport.count, s.lines)
	}
	if !strings.Contains(strings.Join(s.lines, "\n"), "matched and selected") {
		t.Fatalf("confirmed service was not acknowledged: %v", s.lines)
	}
}

// An identical or delayed menu must be validated through the same session path.
func TestFreshPanelAtEndOfFirstOpenWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "menu")

	transport := &stalledPanelTransport{}
	session := gsx.New(transport, dir)
	waits := 0
	session.Sleep = func(ctx context.Context, d time.Duration) error {
		if d == 500*time.Millisecond {
			waits++
			if waits == 8 {
				return os.WriteFile(path, []byte("Ramp\nRequest stairs"), 0600)
			}
		}
		return ctx.Err()
	}
	got, err := session.OpenMenuPage(t.Context())
	if errors.Is(err, gsx.ErrMenuInactive) {
		t.Fatalf("fresh panel published on wait %d was discarded as previous generation; all waits=%d", 8, waits)
	}
	if err != nil || !got.Same(gsx.ParseMenu("Ramp\nRequest stairs")) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// Disable readout after it entered the simulator lane and before the panel opens.
func TestDisablingReadoutCancelsMenuJob(t *testing.T) {
	dir := t.TempDir()
	s := &state{dir: dir, cfg: config.Defaults(), ctx: t.Context(), jobs: make(chan func(context.Context, *gsx.Session), 1)}
	s.cfg.Put(config.MenuReadoutSetting, true)
	s.cfg.Put("gsx_menu_reader_keyword", "read menu")
	s.readouts = newReadoutScope(s.ctx)
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s, Width: 380, Height: 560})
	stubKokoroModel(t, dir)
	transport := &stalledPanelTransport{}
	session := gsx.New(transport, t.TempDir())
	waits := 0
	session.Sleep = func(ctx context.Context, d time.Duration) error {
		if d == 500*time.Millisecond {
			waits++
			if waits == 1 {
				// Apply the Settings save while the simulator waits for Couatl.
				draft := s.cfg.Copy()
				draft.Put(config.MenuReadoutSetting, false)
				s.finishSave(&saveRequest{values: draft}, nil)
			}
		}
		return ctx.Err()
	}
	s.submit("read menu")
	(<-s.jobs)(s.ctx, session)
	if config.Get[bool](s.cfg, config.MenuReadoutSetting) {
		t.Fatal("fixture never disabled readout")
	}
	if waits != 1 || session.RemoteActive() || transport.closes.Load() != 1 {
		t.Fatalf("canceled readout left waits=%d, remote=%t, closes=%d", waits, session.RemoteActive(), transport.closes.Load())
	}
}

// A failed verify must expose repair actions in Settings without reopening it
func TestKokoroVerifyFailureRefreshesOpenSettings(t *testing.T) {
	s := &state{dir: t.TempDir(), cfg: config.Defaults(), ctx: t.Context(), host: gui.NewApp(),
		reader: speech.NewWorker("kokoro")}
	s.cfg.Put(config.MenuReadoutSetting, true)
	s.main = gui.NewTestWindow(t, gui.WindowCfg{})
	stubKokoroModel(t, s.dir)
	s.openSettings()
	cfg := <-s.host.PendingOpen()
	cfg.Height = 3200
	settings := gui.NewTestWindow(t, cfg)
	const repair = "settings-scroll:download-kokoro"
	if _, ok := settings.TestRender(nil).FindByID(repair); ok {
		t.Fatal("model fixture appeared broken on entry")
	}
	// The sized files pass Ready but their contents fail verification.
	s.synthesizeReadout(s.ctx, "One: stairs", 1)
	s.main.FrameFn()
	if _, ok := settings.TestRender(nil).FindByID(repair); !ok {
		t.Fatal("open Settings did not show the repair action")
	}
}

// Match Ready's size and marker checks without downloading a native model
func stubKokoroModel(t *testing.T, dir string) {
	t.Helper()
	model, _ := models.Lookup("kokoro")
	md := model.Directory(dir)
	for _, f := range model.Files {
		path := filepath.Join(md, f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, f.Bytes); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(md, ".verified"), []byte(model.Revision), 0600); err != nil {
		t.Fatal(err)
	}
}
