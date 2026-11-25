// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/ziul510/GSXCommsGo/internal/models"
)

func TestMainMinimizeToTray(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tray       *gui.SystemTrayHandle
		restore    bool
		busy       bool
		wantHidden bool
	}{
		{name: "tray enabled", tray: &gui.SystemTrayHandle{}, wantHidden: true},
		{name: "busy dialog", tray: &gui.SystemTrayHandle{}, busy: true, wantHidden: true},
		{name: "tray unavailable"},
		{name: "restored before hide", tray: &gui.SystemTrayHandle{}, restore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialogClosed := false
			s := &state{tray: tc.tray, dialogs: map[uint64]dialogWindow{
				1: {close: func() { dialogClosed = true }, busy: func() bool { return tc.busy }},
			}}
			w := gui.NewTestWindow(t, gui.WindowCfg{OnEvent: s.mainWindowEvent})
			gui.DispatchWindowOccluded(w, true)
			// Restore before draining the window callbacks to test cancellation of a queued hide.
			if tc.restore {
				gui.DispatchWindowOccluded(w, false)
			}
			w.FrameFn()
			if hidden, wantClosed := !w.IsVisible(), tc.wantHidden && !tc.busy; hidden != tc.wantHidden || dialogClosed != wantClosed {
				t.Fatalf("window hidden=%t, want %t, dialog closed=%t, want %t", hidden, tc.wantHidden, dialogClosed, wantClosed)
			}
		})
	}
}

func TestMainControlsFitAtMinimumWidth(t *testing.T) {
	s := &state{cfg: config.Defaults(), ptt: "Idle"}
	// Include the optional command field in the minimum width layout check
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

func TestMainShowsPTTAvailability(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		speed                               float64
		high, offline, simulation, override bool
		speechLoading, unbound, noAltitude  bool
		activity, want                      string
		color                               gui.Color
	}{
		{name: "at speed limit", speed: 50, want: "Idle", color: ink},
		{name: "above speed limit", speed: 51, want: "Unavailable", color: red},
		{name: "above altitude limit", high: true, want: "Unavailable", color: red},
		{name: "speed override", speed: 51, override: true, want: "Idle", color: ink},
		{name: "speed override at altitude", high: true, override: true, want: "Unavailable", color: red},
		{name: "speed override before altitude arrives", noAltitude: true, override: true, want: "Unavailable", color: red},
		{name: "offline", offline: true, want: "Unavailable", color: red},
		{name: "simulation", offline: true, high: true, simulation: true, want: "Idle", color: ink},
		{name: "speech loading", speechLoading: true, want: "Unavailable", color: red},
		{name: "no binding", unbound: true, want: "Unavailable", color: red},
		{name: "recording", high: true, activity: "LISTENING", want: "LISTENING", color: accent},
		{name: "processing", high: true, activity: "PROCESSING", want: "PROCESSING", color: orange},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &state{cfg: config.Defaults(), connected: !tc.offline, speechReady: !tc.speechLoading, ptt: tc.activity}
			s.cfg.Put("simulation_mode", tc.simulation)
			s.cfg.Put("ignore_speed_restriction", tc.override)
			if tc.unbound {
				s.cfg.Put("ptt_bind", config.Binding{})
			}
			if tc.speechLoading {
				s.speechStatus = "Loading speech"
			}
			s.groundSpeed.Store(&speedReading{tc.speed, time.Now()})
			if !tc.noAltitude {
				altitude := 10000.0
				if tc.high {
					altitude++
				}
				s.pttAltitude.Store(&altitude)
			}
			w := gui.NewTestWindow(t, gui.WindowCfg{State: s, Width: 380, Height: 560})
			w.TestRender(mainView)
			// The fixed label stays visible even when the status is unavailable
			hasText := func(label string, color gui.Color) bool {
				return slices.ContainsFunc(w.Renderers(), func(cmd gui.RenderCmd) bool {
					return cmd.Kind == gui.RenderText && cmd.Text == label && cmd.Color == color
				})
			}
			if !hasText("PTT", ink) || !hasText(tc.want, tc.color) {
				t.Fatalf("missing PTT label or status %q with color %v", tc.want, tc.color)
			}
		})
	}
}

func TestFullJobQueueKeepsControlsUsable(t *testing.T) {
	// An unbuffered queue with no receiver makes every enqueue fail immediately
	s := &state{cfg: config.Defaults(), overlay: true, menu: gsx.ParseMenu("Ramp\nJetway"), jobs: make(chan func(context.Context, *gsx.Session))}
	s.toggleConnect()
	s.toggleMenu()
	s.submit("one")
	if s.busy || !s.overlay || len(s.highlights) != 0 {
		t.Fatalf("busy=%v overlay=%v highlights=%d", s.busy, s.overlay, len(s.highlights))
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
	// Leave the other dialog's write pending when merging this draft.
	s.save(next, nil)
	merged := s.mergeSettings(base, draft)
	if !config.Get[bool](merged, "accent_setup_complete") || config.Get[string](merged, "input_device_name") != "Cockpit" {
		t.Fatal("saving preferences lost a change from another window")
	}
	// Both snapshots must remain usable by the dialogs that already hold them.
	if config.Get[bool](base, "accent_setup_complete") || config.Get[string](next, "input_device_name") == "Cockpit" {
		t.Fatal("the merge modified an existing snapshot")
	}
}

// Exercise the single Top Most toggle through the normal settings save
func TestSettingsTopMostToggle(t *testing.T) {
	s := &state{cfg: config.Defaults(), host: gui.NewApp(), ctx: t.Context(), saves: make(chan *saveRequest, 1)}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	for _, enabled := range []bool{true, false} {
		s.openSettings()
		cfg := <-s.host.PendingOpen()
		cfg.Height = 3200
		w := gui.NewTestWindow(t, cfg)
		w.TestRender(nil)
		if err := w.TestClick("settings-scroll:topmost_active"); err != nil {
			t.Fatal(err)
		}
		if config.Get[bool](s.cfg, "topmost_active") == enabled {
			t.Fatal("Top Most changed before Save Changes")
		}
		if err := w.TestClick("save"); err != nil {
			t.Fatal(err)
		}
		select {
		case req := <-s.saves:
			s.finishSave(req, nil)
		default:
			t.Fatal("Settings did not queue a save")
		}
		if config.Get[bool](s.cfg, "topmost_active") != enabled {
			t.Fatalf("Top Most should be %t after saving", enabled)
		}
	}
}

// Startup downloads must block dismissal even when Settings did not start them
func TestStartupSpeechDownloadBlocksSettingsAndQuit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &state{cfg: config.Defaults(), ctx: ctx, cancel: cancel, host: gui.NewApp(),
		speechLoading: "orukeet", speechStatus: "Downloading 12%"}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{OnCloseRequest: func(*gui.Window) { s.quit() }})
	s.openSettings()
	w := gui.NewTestWindow(t, <-s.host.PendingOpen())
	w.TestRender(nil)
	if err := w.TestClick("save"); !errors.Is(err, gui.ErrTestDisabled) {
		t.Fatalf("Save Changes during startup download: %v", err)
	}
	gui.DispatchCloseRequest(w)
	gui.DispatchCloseRequest(s.main)
	if w.CloseRequested() || s.main.CloseRequested() || ctx.Err() != nil {
		t.Fatal("a close request interrupted the startup download")
	}
	s.speechStatus = "Loading speech"
	gui.DispatchCloseRequest(w)
	gui.DispatchCloseRequest(s.main)
	if !w.CloseRequested() || !s.main.CloseRequested() || ctx.Err() == nil {
		t.Fatal("windows stayed open after the download completed")
	}
}

func TestForceAccentPersistsAfterCrewCues(t *testing.T) {
	const forceKey = "settings-scroll:force_accent"
	s := &state{dir: t.TempDir(), cfg: config.Defaults(), host: gui.NewApp(), ctx: t.Context(), saves: make(chan *saveRequest, 2)}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	commandPrefs := s.cfg.Copy()
	s.cfg.Put("installed_packs", []string{packs[0].name})
	open := func() *gui.Window {
		s.openSettings()
		cfg := <-s.host.PendingOpen()
		cfg.Height = 3200
		w := gui.NewTestWindow(t, cfg)
		w.TestRender(nil)
		return w
	}
	w := open()
	if err := w.TestClick(forceKey); err != nil {
		t.Fatal(err)
	}
	// Scottish is the last accent in the enabled pack's sorted list
	for _, key := range []gui.KeyCode{gui.KeyEnd, gui.KeyEnter} {
		if err := w.TestKey(forceKey, key, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.TestClick("save"); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-s.saves:
		err := config.Save(s.dir, req.values)
		if err != nil {
			t.Fatal(err)
		}
		s.finishSave(req, nil)
	default:
		t.Fatal("Settings did not queue a save")
	}

	s.cue(commandPrefs, "greeting", "", false)
	s.cue(commandPrefs, "boarding", "", false)
	for _, cue := range s.audioQueue {
		candidate := crewAccentCandidates(cue.prefs, nil, nil)[0]
		if _, name := installedCrewAccent(config.Get[[]string](cue.prefs, "installed_packs"), candidate); name != "Scottish" {
			t.Errorf("%s cue preferred %q, want Scottish", cue.cue, name)
		}
	}
	if len(s.audioQueue) != 2 || len(s.saves) != 0 {
		t.Fatal("queuing crew cues did not preserve settings")
	}
	saved, err := config.Load(s.dir)
	if err != nil || config.Get[string](saved, "force_accent") != "Scottish" {
		t.Fatalf("saved force accent = %q, error = %v", config.Get[string](saved, "force_accent"), err)
	}
	for _, cmd := range open().Renderers() {
		if cmd.Kind == gui.RenderText && cmd.Text == "Scottish" {
			return
		}
	}
	t.Fatal("reopened Settings did not show Scottish")
}

func TestMenuReaderDisablesImmediately(t *testing.T) {
	const readKey = config.MenuReadoutSetting
	s := &state{dir: t.TempDir(), cfg: config.Defaults(), host: gui.NewApp(), ctx: t.Context(), saves: make(chan *saveRequest, 2)}
	s.cfg.Put(readKey, true)
	s.cfg.Put("presets", []config.Preset{
		{Name: "Arrival", Codeword: "arrival", Enabled: true, Services: []string{"GPU", config.ReadMenuAction, "JETWAY"}},
		{Name: "Readout", Codeword: "readout", Enabled: true, Services: []string{config.ReadMenuAction}},
	})
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.openSettings()
	cfg := <-s.host.PendingOpen()
	cfg.Height = 3200
	settings := gui.NewTestWindow(t, cfg)
	settings.TestRender(nil)
	for _, cmd := range settings.Renderers() {
		if cmd.Kind == gui.RenderText && cmd.Text == "Kokoro menu reader" {
			t.Fatal("Kokoro still has a separate settings card")
		}
	}
	// Leave a keyword edit unsaved to check that toggling readout saves just the switch.
	if err := settings.TestType("settings-scroll:menu-reader-keyword", "draft keyword"); err != nil {
		t.Fatal(err)
	}
	if err := settings.TestClick("settings-scroll:presets"); err != nil {
		t.Fatal(err)
	}
	manager := gui.NewTestWindow(t, <-s.host.PendingOpen())
	manager.TestRender(nil)
	const dropdown = "presets-scroll:preset-service"
	for _, id := range []string{"new", dropdown} {
		if err := manager.TestClick(id); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []gui.KeyCode{gui.KeyEnd, gui.KeyUp, gui.KeyEnter} {
		if err := manager.TestKey(dropdown, key, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.TestClick("presets-scroll:add-service"); err != nil {
		t.Fatal(err)
	}
	manager.TestRender(nil)
	if !slices.ContainsFunc(manager.Renderers(), func(cmd gui.RenderCmd) bool { return cmd.Text == "1. Read Menu" }) {
		t.Fatal("enabled readout was not selectable in the preset editor")
	}
	if err := settings.TestClick("settings-scroll:" + readKey); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-s.saves:
		if config.Get[bool](req.values, readKey) || config.Get[string](req.values, "gsx_menu_reader_keyword") == "draft keyword" {
			t.Fatal("immediate save did not isolate the readout switch")
		}
		presets := config.Get[[]config.Preset](req.values, "presets")
		if len(presets) != 2 || !slices.Equal(presets[0].Services, []string{"GPU", "JETWAY"}) || len(presets[1].Services) != 0 {
			t.Fatalf("disabling readout did not prune saved sequences: %+v", presets)
		}
		s.finishSave(req, nil)
	default:
		t.Fatal("readout switch did not queue a save")
	}
	if config.Get[bool](s.cfg, readKey) {
		t.Fatal("readout stayed enabled after saving")
	}
	if err := manager.TestClick("presets-scroll:remove-0"); err != nil {
		t.Fatal(err)
	}
	if err := manager.TestClick(dropdown); err != nil {
		t.Fatal(err)
	}
	if err := manager.TestKey(dropdown, gui.KeyEnd, 0); err != nil {
		t.Fatal(err)
	}
	// Check the open editor because disabling readout must update its available actions too.
	if slices.ContainsFunc(manager.Renderers(), func(cmd gui.RenderCmd) bool { return strings.Contains(cmd.Text, config.ReadMenuAction) }) {
		t.Fatal("disabled readout is still in the preset editor")
	}
}

func TestSettingsShowsPresetCardsAfterManagerSave(t *testing.T) {
	s := &state{cfg: config.Defaults(), host: gui.NewApp(), ctx: t.Context(), saves: make(chan *saveRequest, 1)}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.openSettings()
	cfg := <-s.host.PendingOpen()
	// Keep both the Presets section and the manager button in the render viewport
	cfg.Width, cfg.Height = 480, 3200
	settings := gui.NewTestWindow(t, cfg)
	assertPresetSection := func(want ...string) {
		t.Helper()
		settings.TestRender(nil)
		var labels []string
		for _, cmd := range settings.Renderers() {
			if cmd.Kind != gui.RenderText {
				continue
			}
			if cmd.Text == "Presets" || len(labels) > 0 {
				labels = append(labels, cmd.Text)
			}
			if cmd.Text == "Open Preset Manager" {
				break
			}
		}
		if !slices.Equal(labels, want) {
			t.Fatalf("Presets section = %q, want %q", labels, want)
		}
	}
	const description = "Bundle multiple services behind a codeword and pick the run order"
	assertPresetSection("Presets", description, "No presets yet. Open Preset Manager to create one.", "Open Preset Manager")

	if err := settings.TestClick("settings-scroll:presets"); err != nil {
		t.Fatal(err)
	}
	manager := gui.NewTestWindow(t, <-s.host.PendingOpen())
	manager.TestRender(nil)
	if err := manager.TestClick("presets-scroll:create-preset"); err != nil {
		t.Fatal(err)
	}
	if err := manager.TestType("presets-scroll:preset-name", "Arrival services"); err != nil {
		t.Fatal(err)
	}
	if err := manager.TestType("presets-scroll:preset-trigger", "prepare gate"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"preset-enabled", "add-service", "save-preset"} {
		if err := manager.TestClick("presets-scroll:" + id); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case request := <-s.saves:
		s.finishSave(request, nil)
	default:
		t.Fatal("saving the preset did not queue a disk write")
	}
	assertPresetSection("Presets", description, "Arrival services", "prepare gate | Disabled", "Open Preset Manager")
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
	gui.DispatchCloseRequest(settings)
	if settings.CloseRequested() || manager.CloseRequested() || !s.settingsOpen {
		t.Fatal("close request dismissed Settings or its active preset editor")
	}
	gui.DispatchCloseRequest(manager)
	gui.DispatchCloseRequest(settings)
	if !manager.CloseRequested() || !settings.CloseRequested() || s.settingsOpen {
		t.Fatal("Settings did not close after the preset editor closed")
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

func TestSettingsTogglesAlignWithAndWithoutTooltips(t *testing.T) {
	s := &state{cfg: config.Defaults(), host: gui.NewApp(), ctx: t.Context()}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.openSettings()
	settings := gui.NewTestWindow(t, <-s.host.PendingOpen())
	layout := settings.TestRender(nil)
	left, previousY := float32(0), float32(0)
	// Compare neighboring switches so tooltip wrappers cannot shift their positions.
	for i, key := range []string{"debug_mode", "simulation_mode", "ignore_speed_restriction"} {
		node, ok := layout.FindByID("settings-scroll:" + key)
		if !ok {
			t.Fatalf("missing switch %s", key)
		}
		if i == 0 {
			left = node.Shape.X
		} else if node.Shape.X != left || node.Shape.Y-previousY != 40 {
			t.Fatalf("switch %s is misaligned at (%v, %v)", key, node.Shape.X, node.Shape.Y)
		}
		previousY = node.Shape.Y
	}
}

func TestPresetEditorWaitsForPendingChanges(t *testing.T) {
	s := &state{host: gui.NewApp(), cfg: config.Defaults(), saves: make(chan *saveRequest, 1)}
	parent := gui.NewTestWindow(t, gui.WindowCfg{})
	open := func() *gui.Window {
		s.openPresets(parent, func() {})
		window := gui.NewTestWindow(t, <-s.host.PendingOpen())
		window.TestRender(nil)
		return window
	}
	click := func(window *gui.Window, id string) {
		t.Helper()
		if err := window.TestClick(id); err != nil {
			t.Fatal(err)
		}
	}
	add := func(window *gui.Window, name string) *saveRequest {
		t.Helper()
		if err := window.TestType("presets-scroll:preset-name", name); err != nil {
			t.Fatal(err)
		}
		if err := window.TestType("presets-scroll:preset-trigger", strings.ToLower(name)); err != nil {
			t.Fatal(err)
		}
		click(window, "presets-scroll:add-service")
		click(window, "presets-scroll:save-preset")
		select {
		case request := <-s.saves:
			return request
		default:
			t.Fatal("saving the preset did not queue a disk write")
			return nil
		}
	}
	first := open()
	click(first, "presets-scroll:create-preset")
	arrival := add(first, "Arrival")
	gui.DispatchCloseRequest(first)
	if first.CloseRequested() {
		t.Fatal("title-bar close dismissed the preset editor during a save")
	}
	if err := first.TestClick("done"); !errors.Is(err, gui.ErrTestDisabled) {
		t.Fatalf("Done during a save: %v", err)
	}
	s.finishSave(arrival, nil)
	first.TestRender(nil)
	click(first, "done")
	second := open()
	click(second, "new")
	departure := add(second, "Departure")
	s.finishSave(departure, nil)
	presets := config.Get[[]config.Preset](s.cfg, "presets")
	if len(presets) != 2 || presets[0].Codeword != "arrival" || presets[1].Codeword != "departure" || !slices.Equal(presets[0].Services, []string{"GPU"}) {
		t.Fatalf("unexpected presets: %+v", presets)
	}
}

func TestPresetEditorAddsCustomMenuText(t *testing.T) {
	s := &state{host: gui.NewApp(), cfg: config.Defaults(), saves: make(chan *saveRequest, 1)}
	parent := gui.NewTestWindow(t, gui.WindowCfg{})
	s.openPresets(parent, func() {})
	window := gui.NewTestWindow(t, <-s.host.PendingOpen())
	window.TestRender(nil)

	click := func(id string) {
		t.Helper()
		if err := window.TestClick("presets-scroll:" + id); err != nil {
			t.Fatal(err)
		}
	}
	typeText := func(id, text string) {
		t.Helper()
		if err := window.TestType("presets-scroll:"+id, text); err != nil {
			t.Fatal(err)
		}
	}
	click("create-preset")
	typeText("preset-name", "Turnaround")
	typeText("preset-trigger", "turnaround")
	click("add-service")

	const field = "presets-scroll:preset-custom-text"
	if _, shown := window.TestRender(nil).FindByID(field); shown {
		t.Fatal("custom field shown for a built-in service")
	}
	click("preset-service")
	for _, key := range []gui.KeyCode{gui.KeyEnd, gui.KeyEnter} {
		if err := window.TestKey("presets-scroll:preset-service", key, 0); err != nil {
			t.Fatal(err)
		}
	}
	click("add-service")
	typeText("preset-custom-text", "  Cabin Check  ")
	click("add-service")
	typeText("preset-custom-text", "cabin check")
	click("add-service")
	click("save-preset")
	select {
	case request := <-s.saves:
		s.finishSave(request, nil)
	default:
		t.Fatal("saving the preset did not queue a disk write")
	}
	presets := config.Get[[]config.Preset](s.cfg, "presets")
	// Keep custom text in sequence so it can run after the built-in service.
	if len(presets) != 1 || !slices.Equal(presets[0].Services, []string{"GPU", "Custom: Cabin Check"}) {
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
		s.groundSpeed.Store(nil)
		// Use a fresh timestamp to isolate the speed limit from the stale reading check.
		if test.known {
			s.groundSpeed.Store(&speedReading{test.speed, time.Now()})
		}
		if got := s.speedBlock(test.sim, test.override) != ""; got != test.block {
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
	// Age the previous attempt so the disabled setting is the only reason to skip reconnecting.
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

// Zero lets the session exercise its normal closed menu sequence in tests
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
	s := &state{dir: t.TempDir(), ctx: t.Context(), cfg: config.Defaults(), connected: true, speechReady: true,
		overlay: true, menuWatch: true, services: make(map[string]bool), jobs: make(chan func(context.Context, *gsx.Session), 1)}
	s.groundSpeed.Store(&speedReading{0, time.Now()})
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.cfg.Put("auto_select_operator", false)
	s.cfg.Put("mute_gsx_audio_during_crew_voice", false)
	s.menu = gsx.ParseMenu("Ramp\nJetway")
	current := gsx.ParseMenu("Ramp\nRefuel\nJetway")
	tr := &menuChoices{}
	session := gsx.New(tr, "")
	session.ReadMenu = func() gsx.Menu { return current }
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	// A command queued at a valid speed can still be rejected when it runs
	s.submit("one")
	s.groundSpeed.Store(&speedReading{0, time.Now().Add(-3 * time.Second)})
	(<-s.jobs)(s.ctx, session)
	s.main.FrameFn()
	if !strings.Contains(strings.Join(s.lines, "\n"), "Taxi speed unavailable") || len(tr.selected) != 0 {
		t.Fatal("stale speed allowed queued command")
	}
	s.groundSpeed.Store(&speedReading{0, time.Now()})

	// The displayed slot is stale by the time the queued selection runs
	s.submit("one")
	(<-s.jobs)(s.ctx, session)
	if len(tr.selected) != 0 {
		t.Fatal("selected a different item after the menu changed")
	}

	s.main.FrameFn()
	if !s.menu.Same(current) {
		t.Fatal("stale selection did not refresh the displayed page")
	}

	if err := session.Release(s.ctx); err != nil {
		t.Fatal(err)
	}
	// Only Broader Mode should look up the current page for an unrecognized command
	current = gsx.Menu{}
	session.ReadMenu = func() gsx.Menu {
		if tr.open {
			return gsx.ParseMenu("Operators\nSwissport")
		}
		return current
	}
	s.cfg.Put("enable_menu_matching", false)
	s.submit("swisport")
	(<-s.jobs)(s.ctx, session)
	if len(tr.selected) != 0 || tr.open {
		t.Fatal("an unrecognized command opened or selected from the menu with Broader Mode off")
	}
	s.cfg.Put("enable_menu_matching", true)
	s.submit("swisport")
	readMenu := session.ReadMenu
	session.ReadMenu = func() gsx.Menu {
		if tr.open {
			s.groundSpeed.Store(&speedReading{51, time.Now()})
		}
		return readMenu()
	}
	(<-s.jobs)(s.ctx, session)
	if len(tr.selected) != 0 || tr.open {
		t.Fatal("a speed change during correction allowed a choice or left the menu open")
	}
	s.groundSpeed.Store(&speedReading{0, time.Now()})
	session.ReadMenu = readMenu
	s.submit("swisport")
	(<-s.jobs)(s.ctx, session)
	s.main.FrameFn()
	if !slices.Equal(tr.selected, []int{0}) || !strings.Contains(strings.Join(s.lines, "\n"), "swisport → Swissport") {
		t.Fatalf("command did not use the updated menu: %v", tr.selected)
	}
	tr.selected = nil
	current = gsx.ParseMenu("Ramp\nRefuel\nJetway\nRequest cabin check")
	session.ReadMenu = func() gsx.Menu { return current }
	// A preset can share its codeword with a visible menu option
	s.cfg.Put("presets", []config.Preset{{Name: "Ramp", Codeword: "jetway", Enabled: true, Services: []string{"JETWAY", "Custom: Cabin Check", "REFUELING", "JETWAY"}}})
	outer, cancel := context.WithCancel(s.ctx)
	defer cancel()
	// Cancel the outer job on the first choice to check that later services get their own deadline
	tr.afterChoice = cancel
	s.submit("jetway")
	(<-s.jobs)(outer, session)
	if !slices.Equal(tr.selected, []int{1, 2, 0}) {
		t.Fatalf("preset choices=%v", tr.selected)
	}
}

func TestLocalSpeechKeepsOriginalPresetCodeword(t *testing.T) {
	s := &state{dir: t.TempDir(), cfg: config.Defaults(), ctx: t.Context(), speechReady: true,
		jobs: make(chan func(context.Context, *gsx.Session), 1)}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.cfg.Put("simulation_mode", true)
	s.cfg.Put("auto_select_operator", false)
	s.cfg.Put("mute_gsx_audio_during_crew_voice", false)
	s.cfg.Put("presets", []config.Preset{
		{Name: "Fuel", Codeword: "ground power", Enabled: true, Services: []string{"REFUELING"}},
		{Name: "Ramp", Codeword: "ground powder", Enabled: true, Services: []string{"JETWAY"}},
	})
	transport := &menuChoices{}
	session := gsx.New(transport, t.TempDir())
	session.ReadMenu = func() gsx.Menu { return gsx.ParseMenu("Ramp\nGround Power\nJetway\nRefuel") }
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	// This phrase matches one preset before correction and a different one afterward.
	s.submitCommand("ground powder", true)
	(<-s.jobs)(s.ctx, session)
	if !slices.Equal(transport.selected, []int{1}) {
		t.Fatalf("GSX choices = %v, want [1] for Jetway", transport.selected)
	}
}

func TestTypedReadoutPrecedesTransmissionGates(t *testing.T) {
	s := &state{dir: t.TempDir(), cfg: config.Defaults(), ctx: t.Context(),
		jobs: make(chan func(context.Context, *gsx.Session), 1)}
	// Use a speed that would block transmission to check that typed readout is independent.
	s.groundSpeed.Store(&speedReading{80, time.Now()})
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.cfg.Put("read_gsx_menus_aloud_after_keyword", true)
	s.cfg.Put("gsx_menu_reader_keyword", "read menu")
	s.submit("read menu")
	(<-s.jobs)(s.ctx, gsx.New(&menuChoices{}, t.TempDir()))
	s.main.FrameFn()
	log := strings.Join(s.lines, "\n")
	if !strings.Contains(log, "Kokoro menu-reader model is unavailable") || strings.Contains(log, "Taxi speed") {
		t.Fatalf("readout did not reach Kokoro: %s", log)
	}
}

func TestPresetQueuesReadoutBetweenServiceCues(t *testing.T) {
	s := &state{dir: t.TempDir(), ctx: t.Context(), cfg: config.Defaults(), speechReady: true,
		jobs: make(chan func(context.Context, *gsx.Session), 1)}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.cfg.Put(config.MenuReadoutSetting, true)
	s.cfg.Put("simulation_mode", true)
	s.cfg.Put("auto_select_operator", false)
	s.cfg.Put("mute_gsx_audio_during_crew_voice", false)
	s.cfg.Put("presets", []config.Preset{{Name: "Ramp", Codeword: "prepare ramp", Enabled: true,
		Services: []string{"REFUELING", config.ReadMenuAction, "CATERING"}}})
	model, _ := models.Lookup("kokoro")
	// Create a ready-on-disk fixture to test queue order without running Kokoro.
	base := model.Directory(s.dir)
	for _, entry := range model.Files {
		path := filepath.Join(base, entry.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, entry.Bytes); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, ".verified"), []byte(model.Revision), 0600); err != nil {
		t.Fatal(err)
	}
	page := gsx.ParseMenu("Ramp\nRefuel\nCatering")
	transport := &menuChoices{}
	session := gsx.New(transport, t.TempDir())
	session.ReadMenu = func() gsx.Menu { return page }
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	s.submit("prepare ramp")
	(<-s.jobs)(s.ctx, session)
	s.main.FrameFn()
	if !slices.Equal(transport.selected, []int{0, 1}) {
		t.Fatalf("GSX choices = %v", transport.selected)
	}
	queue := s.audioQueue
	if len(queue) != 3 || queue[0].cue != "refueling" || queue[1].readout != "One: Refuel. Two: Catering." || queue[2].cue != "catering" {
		t.Fatalf("preset audio queued out of order: %+v", queue)
	}
	if !strings.Contains(strings.Join(s.lines, "\n"), "2/2 services ok, 1/1 menu readouts queued") {
		t.Fatalf("preset did not report queued readout separately: %v", s.lines)
	}
}

func TestNumberSelectionUsesNormalizedText(t *testing.T) {
	dir := t.TempDir()
	// Map a homophone to one so only the normalized text can select the visible slot.
	if err := os.WriteFile(filepath.Join(dir, "words.json"), []byte(`{"CUSTOM":{"keywords":["one"],"bias_variants":["won"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	page := gsx.ParseMenu("Ramp\nJetway")
	s := &state{dir: dir, cfg: config.Defaults(), ctx: t.Context(), menu: page, overlay: true, menuWatch: true,
		speechReady: true, jobs: make(chan func(context.Context, *gsx.Session), 1)}
	s.groundSpeed.Store(&speedReading{0, time.Now()})
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	transport := &menuChoices{}
	session := gsx.New(transport, t.TempDir())
	session.ReadMenu = func() gsx.Menu { return page }
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	s.cfg.Put("enable_menu_matching", false)
	for _, input := range []string{"won", "option one"} {
		transport.selected = nil
		s.submit(input)
		(<-s.jobs)(s.ctx, session)
		if !slices.Equal(transport.selected, []int{0}) {
			t.Fatalf("%q selected %v", input, transport.selected)
		}
	}
}

func TestExplicitNumberBypassesMenuCorrection(t *testing.T) {
	s := &state{dir: t.TempDir(), cfg: config.Defaults(), ctx: t.Context(), speechReady: true,
		jobs: make(chan func(context.Context, *gsx.Session), 1)}
	s.groundSpeed.Store(&speedReading{0, time.Now()})
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	transport := &menuChoices{}
	session := gsx.New(transport, t.TempDir())
	// Put the digit in a different label to expose accidental fuzzy matching.
	session.ReadMenu = func() gsx.Menu { return gsx.ParseMenu("Ramp\nGate 2\nAdditional Services") }
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	s.submit("option 2")
	(<-s.jobs)(s.ctx, session)
	if !slices.Equal(transport.selected, []int{1}) {
		t.Fatalf("explicit number selected %v", transport.selected)
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

func TestHIDReleaseKeepsOriginalOwnerAfterHotplug(t *testing.T) {
	index := 1
	first := &hidDevice{path: `\\?\hid#vid_1234&pid_5678#first`, name: "Flight Stick"}
	second := &hidDevice{path: `\\?\hid#vid_1234&pid_5678#second`, name: first.name}
	s := &state{cfg: config.Defaults(), ctx: t.Context(), hidPaths: map[string][]string{"flight stick": {first.path}}}
	path, _ := json.Marshal(first.path)
	s.cfg.Put("ptt_bind", config.Binding{Type: "joystick", JoyID: path, JoyName: first.name, JoyButton: &index})
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.hidButton(first, index, true)
	s.main.FrameFn()
	if !s.pttHeld {
		t.Fatal("the only matching device did not start PTT")
	}
	s.hidPaths["flight stick"] = append(s.hidPaths["flight stick"], second.path)
	// Release the same button on the new device to check that ownership stays with the original.
	s.hidButton(second, index, false)
	s.main.FrameFn()
	if !s.pttHeld {
		t.Fatal("a different device released PTT")
	}
	s.hidButton(first, index, false)
	s.main.FrameFn()
	if s.pttHeld {
		t.Fatal("the device holding PTT could not release after hotplug")
	}
}

func TestCapturedBindingSavesImmediately(t *testing.T) {
	original, selecting := captureKey.Load(), selectingKey.Load()
	t.Cleanup(func() { captureKey.Store(original); selectingKey.Store(selecting) })
	captureKey.Store('M')
	s := &state{cfg: config.Defaults(), ctx: t.Context(), host: gui.NewApp(), saves: make(chan *saveRequest, 1)}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.rebind(s.main)
	cfg := <-s.host.PendingOpen()
	dialog := gui.NewTestWindow(t, cfg)
	t.Cleanup(func() { cfg.OnCloseRequest(dialog) })
	s.binding(bindingForKey('N'))
	select {
	case req := <-s.saves:
		// Hold the save callback so the old key must remain active until persistence succeeds.
		gui.DispatchCloseRequest(dialog)
		s.binding(config.Binding{})
		if dialog.CloseRequested() || captureKey.Load() != 'M' {
			t.Fatal("binding changed before the write completed")
		}
		s.finishSave(req, nil)
	default:
		t.Fatal("captured binding did not queue a save")
	}
	if !dialog.CloseRequested() || captureKey.Load() != 'N' || s.binding != nil {
		t.Fatal("saved binding did not take effect and close the dialog")
	}
}

type telemetrySim struct {
	menuChoices
	offline atomic.Bool
}

func (f *telemetrySim) Get(ctx context.Context, key string) (float64, error) {
	// Let the test drop the transport while the telemetry goroutine is reading it.
	if f.offline.Load() {
		return 0, gsx.ErrDisconnected
	}
	return f.menuChoices.Get(ctx, key)
}

func TestTelemetryRunsWhileCommandLaneWaits(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	s := &state{ctx: ctx, cfg: config.Defaults(), done: make(chan struct{}), jobs: make(chan func(context.Context, *gsx.Session), 2),
		debug: make(chan debugEvent, 16)}
	s.debugEnabled.Store(true)
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.overlay, s.connected = true, true
	s.menu = gsx.ParseMenu("Ramp\nJetway")
	s.highlights = []menuHighlight{{page: s.menu, index: 0, until: time.Now().Add(time.Second)}}
	go s.simulator()
	t.Cleanup(func() {
		cancel()
		<-s.done
	})

	tr := &telemetrySim{}
	s.jobs <- func(_ context.Context, session *gsx.Session) { session.Sim = tr }
	// Hold a command open while watching for fresh telemetry readings.
	started := make(chan struct{})
	s.jobs <- func(ctx context.Context, _ *gsx.Session) {
		close(started)
		<-ctx.Done()
	}
	<-started
	deadline := time.Now().Add(3 * time.Second)
	var first *speedReading
	for time.Now().Before(deadline) {
		if reading := s.groundSpeed.Load(); reading != nil {
			if first != nil && first != reading {
				tr.offline.Store(true)
				break
			}
			first = reading
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !tr.offline.Load() {
		t.Fatal("command lane blocked telemetry")
	}
	for time.Now().Before(deadline) {
		s.main.FrameFn()
		if !s.connected {
			if s.overlay || len(s.menu.Options) != 0 || len(s.highlights) != 0 || s.groundSpeed.Load() != nil {
				t.Fatal("disconnect kept stale menu or telemetry")
			}
			var output strings.Builder
			for len(s.debug) > 0 {
				fmt.Fprintln(&output, (<-s.debug).line)
			}
			for _, want := range []string{"Waiting for GSX/Couatl to start", "Ground speed available", "SimConnect disconnected"} {
				if strings.Count(output.String(), want) != 1 {
					t.Errorf("expected one %q event, got:\n%s", want, output.String())
				}
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("command lane blocked disconnect status")
}

func TestDebugDiagnosticsExcludeCredentialsAndTranscripts(t *testing.T) {
	const key = "fixture-credential"
	s := &state{cfg: config.Defaults(), debug: make(chan debugEvent, 16)}
	// Store the uppercase credential to test case-insensitive redaction.
	s.setLogSecret(strings.ToUpper(key))
	s.cfg.Put("speech_provider", key)
	s.cfg.Put("ptt_bind", config.Binding{Type: key})
	s.cfg.Put("input_device_name", key)
	s.setDebug(true)
	s.log("CMD", "transcript "+key)
	s.log("ERR", "error response "+key)
	s.traceFailure("SIM", "Handshake failed", fmt.Errorf("%s: %w", key, context.DeadlineExceeded))
	s.traceFailure("AUDIO", "Playback failed", errors.New(key))
	if strings.Contains(strings.Join(s.lines, "\n"), key) {
		t.Fatal("conversation exposed the saved credential")
	}
	var output strings.Builder
	for len(s.debug) > 0 {
		event := <-s.debug
		if strings.Contains(event.line, key) || strings.Contains(event.line, "transcript") || strings.Contains(event.line, "error response") {
			t.Fatal("debug writer received user or credential text")
		}
		fmt.Fprintln(&output, event.line)
	}
	for _, want := range []string{"INFO  SYS    GSXComms debug session", "Provider=unknown", "Binding=unknown",
		"WARN  SIM    Handshake failed: timed out", "WARN  AUDIO  Playback failed: see the in-app message for details"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q in debug output:\n%s", want, output.String())
		}
	}
}

func TestLegacyHIDNameRequiresRebindingAndNativePathCanMove(t *testing.T) {
	const original = "\\\\?\\hid#vid_1234&pid_1234#first"
	const current = "\\\\?\\hid#vid_1234&pid_1234#second"
	index := 5
	s := &state{hidPaths: map[string][]string{"stick": {current}}}
	binding := config.Binding{Type: "joystick", JoyName: "Stick", JoyButton: &index, Description: "Stick button 6"}
	if description, bound := s.bindingDescription(binding); bound || !strings.Contains(description, "Unverified") || s.boundHIDPath(binding) != "" {
		t.Fatalf("legacy name activated a binding: %q", description)
	}
	binding.JoyID, _ = json.Marshal(original)
	if got := s.boundHIDPath(binding); got != current {
		t.Fatalf("unique VID/PID matched %q", got)
	}
	// Add a second device with the same identifiers to make the fallback ambiguous.
	s.hidPaths["stick"] = append(s.hidPaths["stick"], current+"-duplicate")
	if got := s.boundHIDPath(binding); got != "" {
		t.Fatalf("ambiguous VID/PID matched %q", got)
	}
}

func TestPresetSummaryExpandsIntoExistingEditor(t *testing.T) {
	s := &state{host: gui.NewApp(), cfg: config.Defaults()}
	s.cfg.Put("presets", []config.Preset{{Name: "Arrival", Codeword: "arrival", Services: []string{"GPU", "JETWAY"}}})
	parent := gui.NewTestWindow(t, gui.WindowCfg{})
	s.openPresets(parent, func() {})
	window := gui.NewTestWindow(t, <-s.host.PendingOpen())
	layout := window.TestRender(nil)
	if _, visible := layout.FindByID("presets-scroll:edit-preset-0"); visible {
		t.Fatal("collapsed summary exposed Edit")
	}
	if err := window.TestClick("presets-scroll:preset-0"); err != nil {
		t.Fatal(err)
	}
	if err := window.TestClick("presets-scroll:edit-preset-0"); err != nil {
		t.Fatal(err)
	}
	if _, visible := window.TestRender(nil).FindByID("presets-scroll:preset-name"); !visible {
		t.Fatal("expanded summary did not open the existing editor")
	}
	if err := window.TestClick("presets-scroll:down-0"); err != nil {
		t.Fatal(err)
	}
	window.TestRender(nil)
	// Check the rendered numbers after a reorder so the editor shows the new sequence.
	var numbered []string
	for _, cmd := range window.Renderers() {
		if cmd.Kind == gui.RenderText && (cmd.Text == "1. JETWAY" || cmd.Text == "2. GPU") {
			numbered = append(numbered, cmd.Text)
		}
	}
	if !slices.Equal(numbered, []string{"1. JETWAY", "2. GPU"}) {
		t.Fatalf("editor did not renumber reordered services: %v", numbered)
	}
}

type presetConfirmPlatform struct {
	gui.NativePlatform
	status gui.NativeDialogStatus
	body   string
}

func (*presetConfirmPlatform) TitlebarDark(bool)                                {}
func (*presetConfirmPlatform) SetSystemAppearanceCallback(func(gui.Appearance)) {}
func (*presetConfirmPlatform) A11yDestroy()                                     {}

func (p *presetConfirmPlatform) ShowConfirmDialog(_, body string, _ gui.NativeAlertLevel) gui.NativeAlertResult {
	p.body = body
	return gui.NativeAlertResult{Status: p.status}
}

func TestPresetCardDeleteRequiresConfirmation(t *testing.T) {
	s := &state{host: gui.NewApp(), cfg: config.Defaults(), saves: make(chan *saveRequest, 1)}
	s.cfg.Put("presets", []config.Preset{
		{Name: "Arrival", Codeword: "arrival", Services: []string{"GPU"}},
		{Name: "Departure", Codeword: "departure", Services: []string{"JETWAY"}},
	})
	parent := gui.NewTestWindow(t, gui.WindowCfg{})
	s.openPresets(parent, func() {})
	window := gui.NewTestWindow(t, <-s.host.PendingOpen())
	platform := &presetConfirmPlatform{status: gui.DialogCancel}
	window.SetNativePlatform(platform)
	layout := window.TestRender(nil)
	const deleteID = "presets-scroll:delete-preset-0"
	card, hasCard := layout.FindByID("presets-scroll:preset-0")
	deleteButton, hasDelete := layout.FindByID(deleteID)
	if !hasCard || !hasDelete || deleteButton.Shape.X < card.Shape.X+card.Shape.Width {
		t.Fatal("Delete button does not sit to the right of the collapsed preset")
	}
	if err := window.TestClick(deleteID); err != nil {
		t.Fatal(err)
	}
	if platform.body != "Delete Arrival?" || len(s.saves) != 0 {
		t.Fatalf("canceling %q queued %d writes", platform.body, len(s.saves))
	}
	// Repeat the delete with confirmation to check that only acceptance queues a write.
	platform.status = gui.DialogOK
	if err := window.TestClick(deleteID); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-s.saves:
		s.finishSave(request, nil)
	default:
		t.Fatal("confirming preset deletion did not queue a save")
	}
	if presets := config.Get[[]config.Preset](s.cfg, "presets"); len(presets) != 1 || presets[0].Name != "Departure" {
		t.Fatalf("unexpected presets: %+v", presets)
	}
}
