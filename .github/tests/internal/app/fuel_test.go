// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/gsx"
)

type fuelMenuTransport struct {
	menuChoices
	menuWrites int
}

// Count flag writes because reopening the menu can dismiss the fuel prompt.
func (f *fuelMenuTransport) Set(ctx context.Context, name string, value float64) error {
	if name == gsx.MenuOpen || name == gsx.Remote {
		f.menuWrites++
	}
	return f.menuChoices.Set(ctx, name, value)
}

// Exercise routing through the same command entry point as speech recognition.
func TestFuelQuantityCommandUsesPendingGSXMenu(t *testing.T) {
	const full = "Fuel to 100% (24,000 lbs)"
	fuel := gsx.ParseMenu("Select refueling level\n10%\n25%\n40%\n50%\n65%\n80%\n" + full)
	ramp := gsx.ParseMenu("Ramp\nRequest refueling\nRequest boarding")
	for _, tt := range []struct {
		name, input string
		snapshot    gsx.Menu
		duplicate   bool
		changePage  bool
		want        []int
	}{
		{name: "bare number without overlay", input: "Seven.", want: []int{6}},
		{name: "number with overlay", input: "option seven", snapshot: fuel, want: []int{6}},
		{name: "bare percentage", input: "100", want: []int{6}},
		{name: "spoken percentage", input: "100 percent", want: []int{6}},
		{name: "word percentage", input: "one hundred per cent", want: []int{6}},
		{name: "full label contains fuel", input: full, want: []int{6}},
		{name: "spoken full label", input: "Fuel to one hundred percent, 24,000 lbs", want: []int{6}},
		{name: "duplicate percentage", input: "100 percent", duplicate: true},
		{name: "full label disambiguates percentage", input: full, duplicate: true, want: []int{6}},
		{name: "unavailable quantity", input: "99 percent"},
		{name: "unavailable slot", input: "option nine"},
		{name: "stale overlay slot", input: "Seven", snapshot: ramp},
		{name: "page changes before write", input: "100", changePage: true},
		{name: "ambiguous service command", input: "board and refuel"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &state{dir: t.TempDir(), ctx: t.Context(), cfg: config.Defaults(), speechReady: true,
				jobs: make(chan func(context.Context, *gsx.Session), 1)}
			s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
			s.cfg.Put("simulation_mode", true)
			s.cfg.Put("enable_menu_matching", false)
			s.cfg.Put("mute_gsx_audio_during_crew_voice", false)
			transport := &fuelMenuTransport{}
			session := gsx.New(transport, t.TempDir())
			current := ramp
			session.ReadMenu = func() gsx.Menu { return current }
			session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

			// Refueling starts on the ramp menu. The truck can arrive after idle release.
			s.submitCommand("request refueling", true)
			(<-s.jobs)(s.ctx, session)
			if !slices.Equal(transport.selected, []int{0}) {
				t.Fatalf("refueling selected %v, want [0]", transport.selected)
			}
			if err := session.Release(s.ctx); err != nil {
				t.Fatal(err)
			}
			transport.selected, transport.menuWrites = nil, 0
			current = fuel
			if tt.duplicate {
				current.Options = append(slices.Clone(fuel.Options), "100% (SimBrief block fuel)")
			}
			s.menu = tt.snapshot
			s.overlay, s.menuWatch = tt.snapshot.Active, tt.snapshot.Active
			if tt.changePage {
				// Let routing see the prompt, then let the write guard see the new page.
				session.ReadMenu = func() gsx.Menu {
					page := current
					current = ramp
					return page
				}
			}
			s.submitCommand(tt.input, true)
			(<-s.jobs)(s.ctx, session)
			if !slices.Equal(transport.selected, tt.want) || transport.menuWrites != 0 || session.RemoteActive() {
				t.Fatalf("selected %v, want %v, menu flag writes %d, owns menu %v",
					transport.selected, tt.want, transport.menuWrites, session.RemoteActive())
			}
		})
	}
}
