// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
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

func TestMainControlsFitAtMinimumWidth(t *testing.T) {
	s := &state{cfg: config.Defaults(), ptt: "Idle"}
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

func TestMainShowsPTTAvailability(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		speed                               float64
		high, offline, simulation, override bool
		speechLoading, unbound              bool
		activity, want                      string
		color                               gui.Color
	}{
		{name: "at speed limit", speed: 50, want: "PTT Idle", color: ink},
		{name: "above speed limit", speed: 51, want: "Unavailable", color: red},
		{name: "above altitude limit", high: true, want: "Unavailable", color: red},
		{name: "speed override", speed: 51, override: true, want: "PTT Idle", color: ink},
		{name: "speed override at altitude", high: true, override: true, want: "Unavailable", color: red},
		{name: "offline", offline: true, want: "Unavailable", color: red},
		{name: "simulation", offline: true, high: true, simulation: true, want: "PTT Idle", color: ink},
		{name: "speech loading", speechLoading: true, want: "Unavailable", color: red},
		{name: "no binding", unbound: true, want: "Unavailable", color: red},
		{name: "recording", high: true, activity: "LISTENING", want: "PTT LISTENING", color: accent},
		{name: "processing", high: true, activity: "PROCESSING", want: "PTT PROCESSING", color: orange},
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
			s.abovePTTAltitude.Store(tc.high)
			w := gui.NewTestWindow(t, gui.WindowCfg{State: s, Width: 380, Height: 560})
			w.TestRender(mainView)
			// Check the visible status text and color together
			for _, cmd := range w.Renderers() {
				if cmd.Kind == gui.RenderText && cmd.Text == tc.want && cmd.Color == tc.color {
					return
				}
			}
			t.Fatalf("missing rendered PTT status %q with color %v", tc.want, tc.color)
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
	s.save(next, nil)
	merged := s.mergeSettings(base, draft)
	if !config.Get[bool](merged, "accent_setup_complete") || config.Get[string](merged, "input_device_name") != "Cockpit" {
		t.Fatal("saving preferences lost a change from another window")
	}
	if config.Get[bool](base, "accent_setup_complete") || config.Get[string](next, "input_device_name") == "Cockpit" {
		t.Fatal("the merge modified an existing snapshot")
	}
}

func TestMenuReaderDisablesImmediately(t *testing.T) {
	const readKey = "read_gsx_menus_aloud_after_keyword"
	s := &state{dir: t.TempDir(), cfg: config.Defaults(), host: gui.NewApp(), ctx: t.Context(), saves: make(chan *saveRequest, 2)}
	s.cfg.Put(readKey, true)
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
	if err := settings.TestType("settings-scroll:menu-reader-keyword", "draft keyword"); err != nil {
		t.Fatal(err)
	}
	if err := settings.TestClick("settings-scroll:" + readKey); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-s.saves:
		if config.Get[bool](req.values, readKey) || config.Get[string](req.values, "gsx_menu_reader_keyword") == "draft keyword" {
			t.Fatal("immediate save did not isolate the readout switch")
		}
		s.finishSave(req, nil)
	default:
		t.Fatal("readout switch did not queue a save")
	}
	if config.Get[bool](s.cfg, readKey) {
		t.Fatal("readout stayed enabled after saving")
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

func TestSettingsTogglesAlignWithAndWithoutTooltips(t *testing.T) {
	s := &state{cfg: config.Defaults(), host: gui.NewApp(), ctx: t.Context()}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.openSettings()
	settings := gui.NewTestWindow(t, <-s.host.PendingOpen())
	layout := settings.TestRender(nil)
	left, previousY := float32(0), float32(0)
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

func TestPresetEditorReopensWithPendingChanges(t *testing.T) {
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
	click(first, "done")
	// Reopen before the editor receives its first write completion.
	second := open()
	click(second, "new")
	departure := add(second, "Departure")
	s.finishSave(arrival, nil)
	s.finishSave(departure, nil)
	presets := config.Get[[]config.Preset](s.cfg, "presets")
	if len(presets) != 2 || presets[0].Codeword != "arrival" || presets[1].Codeword != "departure" || !slices.Equal(presets[0].Services, []string{"GPU"}) {
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
	current = gsx.ParseMenu("Ramp\nRefuel\nJetway")
	session.ReadMenu = func() gsx.Menu { return current }
	// A preset can share its codeword with a visible menu option
	s.cfg.Put("presets", []config.Preset{{Name: "Ramp", Codeword: "jetway", Enabled: true, Services: []string{"JETWAY", "REFUELING", "JETWAY"}}})
	outer, cancel := context.WithCancel(s.ctx)
	defer cancel()
	// Cancel the outer job on the first choice to check that later services get their own deadline
	tr.afterChoice = cancel
	s.submit("jetway")
	(<-s.jobs)(outer, session)
	if !slices.Equal(tr.selected, []int{1, 0}) {
		t.Fatalf("preset choices=%v", tr.selected)
	}
}

func TestTypedReadoutPrecedesTransmissionGates(t *testing.T) {
	s := &state{dir: t.TempDir(), cfg: config.Defaults(), ctx: t.Context(),
		jobs: make(chan func(context.Context, *gsx.Session), 1)}
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

func TestNumberSelectionUsesNormalizedText(t *testing.T) {
	dir := t.TempDir()
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
	s.submit("won")
	(<-s.jobs)(s.ctx, session)
	if !slices.Equal(transport.selected, []int{0}) {
		t.Fatalf("normalized number selected %v", transport.selected)
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
	if f.offline.Load() {
		return 0, gsx.ErrDisconnected
	}
	return f.menuChoices.Get(ctx, key)
}

func TestTelemetryRunsWhileCommandLaneWaits(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	s := &state{ctx: ctx, cfg: config.Defaults(), done: make(chan struct{}), jobs: make(chan func(context.Context, *gsx.Session), 2)}
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
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("command lane blocked disconnect status")
}

func TestDebugDiagnosticsExcludeCredentialsAndTranscripts(t *testing.T) {
	const key = "fixture-credential"
	s := &state{cfg: config.Defaults(), debug: make(chan debugEvent, 16)}
	s.setLogSecret(strings.ToUpper(key))
	s.cfg.Put("speech_provider", key)
	s.setDebug(true)
	s.log("CMD", "transcript "+key)
	s.log("ERR", "error response "+key)
	s.traceFailure("GSX", "Simulator stopped")
	if strings.Contains(strings.Join(s.lines, "\n"), key) {
		t.Fatal("conversation exposed the saved credential")
	}
	for len(s.debug) > 0 {
		event := <-s.debug
		if strings.Contains(event.line, key) || strings.Contains(event.line, "transcript") || strings.Contains(event.line, "error response") {
			t.Fatal("debug writer received user or credential text")
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
}
