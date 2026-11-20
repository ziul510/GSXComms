// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/gsx"
)

func TestMainControlsFitAtMinimumWidth(t *testing.T) {
	s := &state{cfg: config.Defaults(), ptt: "Idle", highlight: -1}
	// Include the optional command field in the minimum-width layout check
	s.cfg.Put("enable_keyboard_input", true)
	w := gui.NewTestWindow(t, gui.WindowCfg{State: s, Width: 380, Height: 560})
	layout := w.TestRender(mainView)
	for _, id := range []string{"connect", "gsx-menu", "settings", "command"} {
		node, ok := layout.FindByID(id)
		if !ok {
			t.Fatalf("missing widget %s", id)
		}
		shape := node.Shape
		if shape.Width <= 0 || shape.X < 0 || shape.X+shape.Width > 380 {
			t.Errorf("%s exceeds the window: x=%f width=%f", id, shape.X, shape.Width)
		}
	}
	if found := w.TestFindings(gui.DebugDuplicates | gui.DebugMissingIDs); len(found) > 0 {
		t.Fatalf("widget IDs: %v", found)
	}
}

func TestFullJobQueueKeepsControlsUsable(t *testing.T) {
	// An unbuffered queue with no receiver makes every enqueue fail immediately
	s := &state{cfg: config.Defaults(), overlay: true, menu: gsx.ParseMenu("Ramp\nJetway"), highlight: -1, jobs: make(chan func(context.Context, *gsx.Session))}
	s.toggleConnect()
	s.toggleMenu()
	s.choose(0)
	if s.busy || !s.overlay || s.highlight != -1 {
		t.Fatalf("busy=%v overlay=%v highlight=%d", s.busy, s.overlay, s.highlight)
	}
}

func TestSettingsDraftPreservesOtherWindowChanges(t *testing.T) {
	s := &state{cfg: config.Defaults(), saves: make(chan *saveRequest, 2)}
	// Take the opening snapshot before simulating a save from a separate dialog
	base := s.settingsDraft()
	draft := base.Copy()
	draft.Put("input_device_name", "Cockpit")
	next := s.settingsDraft()
	next.Put("accent_setup_complete", true)
	s.save(next, nil)
	merged := s.mergeSettings(base, draft)
	if !config.Get[bool](merged, "accent_setup_complete") || config.Get[string](merged, "input_device_name") != "Cockpit" {
		t.Fatal("saving preferences lost a change from another window")
	}
	if config.Get[bool](base, "accent_setup_complete") || config.Get[string](next, "input_device_name") == "Cockpit" {
		t.Fatal("the merge modified an existing snapshot")
	}
}

func TestSettingsOwnsPresetManager(t *testing.T) {
	s := &state{cfg: config.Defaults(), host: gui.NewApp(), ctx: t.Context(), saves: make(chan *saveRequest, 1)}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.openSettings()
	cfg := <-s.host.PendingOpen()
	// Make the full Settings panel visible for direct widget clicks
	cfg.Height = 3200
	settings := gui.NewTestWindow(t, cfg)
	settings.TestRender(nil)
	if err := settings.TestClick("settings-scroll:presets"); err != nil {
		t.Fatal(err)
	}
	// Consume the first pending dialog so any later request would be a duplicate
	manager := gui.NewTestWindow(t, <-s.host.PendingOpen())
	if err := settings.TestClick("settings-scroll:presets"); !errors.Is(err, gui.ErrTestDisabled) {
		t.Fatalf("preset manager button: %v", err)
	}
	if err := settings.TestClick("save"); !errors.Is(err, gui.ErrTestDisabled) {
		t.Fatalf("settings save button: %v", err)
	}
	if len(s.saves) != 0 || len(s.host.PendingOpen()) != 0 {
		t.Fatal("opened a second editor or saved the draft with the first editor open")
	}
	cfg.OnCloseRequest(settings)
	if !manager.CloseRequested() || s.settingsOpen {
		t.Fatal("closing Settings left the preset editor open")
	}
}

func TestSettingsSlidersUseTheAvailableWidth(t *testing.T) {
	s := &state{cfg: config.Defaults(), host: gui.NewApp(), ctx: t.Context()}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.openSettings()
	cfg := <-s.host.PendingOpen()
	cfg.Width, cfg.Height = 480, 680
	settings := gui.NewTestWindow(t, cfg)
	layout := settings.TestRender(nil)
	// Require the sliders to fill the settings column without extending beyond the window
	for _, key := range []string{"volume_static", "volume_voice", "max_recording_seconds"} {
		node, ok := layout.FindByID("settings-scroll:" + key)
		if !ok || node.Shape.Width < 400 || node.Shape.X+node.Shape.Width > 480 {
			t.Fatalf("%s does not fill the settings column", key)
		}
	}
}

func TestPresetEmptyStateCreatesPreset(t *testing.T) {
	s := &state{host: gui.NewApp(), cfg: config.Defaults(), saves: make(chan *saveRequest, 1)}
	parent := gui.NewTestWindow(t, gui.WindowCfg{})
	draft := config.Defaults()
	s.openPresets(draft, parent, func() {})
	manager := gui.NewTestWindow(t, <-s.host.PendingOpen())
	manager.TestRender(nil)
	click := func(id string) {
		t.Helper()
		if err := manager.TestClick("presets-scroll:" + id); err != nil {
			t.Fatal(err)
		}
	}
	// Use the actual empty-state action so this exercises the editor callbacks
	click("create-preset")
	if err := manager.TestType("presets-scroll:preset-name", "Arrival"); err != nil {
		t.Fatal(err)
	}
	if err := manager.TestType("presets-scroll:preset-trigger", "arrival"); err != nil {
		t.Fatal(err)
	}
	// A fresh form starts with GPU as the service picker value
	click("add-service")
	click("save-preset")
	select {
	case request := <-s.saves:
		s.finishSave(request, nil)
	default:
		t.Fatal("saving the preset did not queue a disk write")
	}
	presets := config.Get[[]config.Preset](draft, "presets")
	if len(presets) != 1 || presets[0].Codeword != "arrival" || !slices.Equal(presets[0].Services, []string{"GPU"}) {
		t.Fatalf("unexpected presets: %+v", presets)
	}
}

func TestTransmissionSpeedGate(t *testing.T) {
	s := &state{cfg: config.Defaults()}
	for _, test := range []struct {
		known, override, sim bool
		speed                float64
		block                bool
	}{
		// Cover unknown speed, the exact limit, and both explicit bypasses
		{false, false, false, 0, true},
		{true, false, false, 50, false},
		{true, false, false, 51, true},
		{false, true, false, 0, false},
		{false, false, true, 0, false},
	} {
		s.speedKnown, s.speed = test.known, test.speed
		s.cfg.Put("ignore_speed_restriction", test.override)
		s.cfg.Put("simulation_mode", test.sim)
		if got := s.transmissionBlock() != ""; got != test.block {
			t.Errorf("speed gate for %+v blocked=%v", test, got)
		}
	}
}

func TestOfflineAutoConnectWaitsAndQueuesOneAttempt(t *testing.T) {
	s := &state{cfg: config.Defaults(), jobs: make(chan func(context.Context, *gsx.Session), 2), lastConnect: time.Now()}
	s.retryConnection()
	if len(s.jobs) != 0 {
		t.Fatal("retried within the connection cooldown")
	}
	s.lastConnect = time.Now().Add(-16 * time.Second)
	s.cfg.Put("auto_connect_sim", false)
	s.retryConnection()
	if len(s.jobs) != 0 {
		t.Fatal("queued an automatic connection after the user disabled it")
	}
	s.cfg.Put("auto_connect_sim", true)
	s.retryConnection()
	s.retryConnection()
	if !s.busy || len(s.jobs) != 1 {
		t.Fatalf("queued %d attempts, busy=%v", len(s.jobs), s.busy)
	}
}

type menuChoices struct {
	selected    []int
	afterChoice func()
	open        bool
}

// Zero lets the session exercise its normal closed-menu sequence in tests
func (*menuChoices) Get(ctx context.Context, _ string) (float64, error) { return 0, ctx.Err() }
func (m *menuChoices) Set(ctx context.Context, name string, value float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Record only choices Remote and open flag writes aren't part of this assertion
	if name == gsx.MenuChoice {
		m.selected = append(m.selected, int(value))
		if m.afterChoice != nil {
			m.afterChoice()
		}
	}
	if name == gsx.MenuOpen {
		m.open = value > 0
	}
	return nil
}
func (*menuChoices) Close() error { return nil } // There is no native simulator handle in this fixture

func TestCommandUsesCurrentMenuAndPreservesPresetOrder(t *testing.T) {
	s := &state{dir: t.TempDir(), ctx: t.Context(), cfg: config.Defaults(), connected: true, speedKnown: true, speechReady: true,
		menuWatch: true, services: make(map[string]bool), jobs: make(chan func(context.Context, *gsx.Session), 1), audioQueue: make(chan playback, 3)}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.cfg.Put("auto_select_operator", false)
	s.cfg.Put("mute_gsx_audio_during_crew_voice", false)
	s.cfg.Put("enable_gsx_menu_text_correction", true)
	s.menu = gsx.ParseMenu("Ramp\nJetway")
	current := gsx.ParseMenu("Ramp\nRefuel\nJetway")
	tr := &menuChoices{}
	session := gsx.New(tr, "")
	session.ReadMenu = func() gsx.Menu { return current }
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	// The displayed slot is stale by the time the queued selection runs
	s.choose(0)
	(<-s.jobs)(s.ctx, session)
	if len(tr.selected) != 0 {
		t.Fatal("selected a different item after the menu changed")
	}

	s.cfg.Put("enable_menu_matching", false)
	// Text correction must fetch the current page even when broader matching is disabled
	current = gsx.Menu{}
	session.ReadMenu = func() gsx.Menu {
		if tr.open {
			return gsx.ParseMenu("Operators\nSwissport")
		}
		return current
	}
	s.submit("Swissport")
	(<-s.jobs)(s.ctx, session)
	if !slices.Equal(tr.selected, []int{0}) {
		t.Fatalf("command did not use the updated menu: %v", tr.selected)
	}
	tr.selected = nil
	current = gsx.ParseMenu("Ramp\nRefuel\nJetway")
	session.ReadMenu = func() gsx.Menu { return current }
	s.cfg.Put("enable_gsx_menu_text_correction", false)
	s.cfg.Put("presets", []config.Preset{{Name: "Ramp", Codeword: "turnaround", Enabled: true, Services: []string{"JETWAY", "REFUELING", "JETWAY"}}})
	outer, cancel := context.WithCancel(s.ctx)
	defer cancel()
	// Cancel the outer job on the first choice to check that later services get their own deadline
	tr.afterChoice = cancel
	s.submit("turnaround")
	(<-s.jobs)(outer, session)
	if !slices.Equal(tr.selected, []int{1, 0}) {
		t.Fatalf("preset choices=%v", tr.selected)
	}
}

func TestMenuReadoutFormatting(t *testing.T) {
	// Use actual panel labels to check number formatting and punctuation cleanup together
	options := gsx.ParseMenu("GSX\n1. Request de-icing\n2. Stand B-12")
	want := "One: Request de icing. Two: Stand B 12."
	if got := formatReadout(options.Options); got != want {
		t.Fatalf("readout=%q", got)
	}
}

func TestLaterSuccessfulSaveAppliesBinding(t *testing.T) {
	original := captureKey.Load()
	t.Cleanup(func() { captureKey.Store(original) })
	s := &state{cfg: config.Defaults(), saves: make(chan *saveRequest, 2), pttHeld: true}
	captureKey.Store('M')
	next := s.settingsDraft()
	next.Put("ptt_bind", bindingForKey('N'))
	s.save(next, nil)
	later := s.settingsDraft()
	later.Put("gsx_hotfix_completed", true)
	s.save(later, nil)
	// A failed earlier write leaves the old PTT key active until the later save succeeds
	s.finishSave(<-s.saves, errors.New("settings file busy"))
	if captureKey.Load() != 'M' || !s.pttHeld {
		t.Fatal("failed save changed the active binding")
	}
	s.finishSave(<-s.saves, nil)
	if captureKey.Load() != 'N' || s.pttHeld || !config.Get[bool](s.cfg, "gsx_hotfix_completed") {
		t.Fatal("successful save left runtime settings stale")
	}
}
