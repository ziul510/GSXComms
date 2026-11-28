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

// Share the real overlay job queue and render state with a fake GSX transport
func testMenuState(t *testing.T) (*state, *sessionMenuCache) {
	t.Helper()
	s := &state{ctx: t.Context(), cfg: config.Defaults(), connected: true, ready: true,
		jobs: make(chan func(context.Context, *gsx.Session), 2)}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s, Width: 380, Height: 560})
	cache := &sessionMenuCache{}
	s.menuCache.Store(cache)
	return s, cache
}

// Exercise read and choice operations while the overlay is hidden
func TestMenuReadsReachOverlayAndSurviveIdle(t *testing.T) {
	s, cache := testMenuState(t)
	tr := &menuChoices{}
	session := gsx.New(tr, "")
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	parent := gsx.ParseMenu("Ramp\nAdditional Services")
	submenu := gsx.ParseMenu("Additional Services\nRequest inspection")
	page := parent
	session.ReadMenu = func() gsx.Menu { return page }
	s.observeMenuReads(session)
	tr.afterChoice = func() { page = submenu }
	if _, err := session.Execute(s.ctx, "request inspection", "", nil, false); err != nil {
		t.Fatal(err)
	}
	if err := session.Release(s.ctx); err != nil {
		t.Fatal(err)
	}
	page = gsx.Menu{}
	session.ReadMenu()
	s.main.FrameFn()
	if s.overlay || !s.menu.Same(submenu) {
		t.Fatalf("hidden overlay did not retain submenu: %+v", s.menu)
	}
	at := cache.latest.Load().at
	s.rememberMenu(cache, submenu, at.Add(15*time.Second), false)
	if got := cache.latest.Load(); got.fresh(at.Add(15*time.Second)) || !got.fresh(at.Add(15*time.Second-time.Nanosecond)) {
		t.Fatal("unchanged idle poll extended the cache or broke the 15-second cutoff")
	}
	s.toggleMenu()
	if !s.overlay || !s.menu.Same(submenu) {
		t.Fatal("fresh page was not visible before the simulator could handle the job")
	}
	s.main.TestRender(mainView)
	if !slices.ContainsFunc(s.main.Renderers(), func(cmd gui.RenderCmd) bool {
		return cmd.Kind == gui.RenderText && cmd.Text == "1. Request inspection"
	}) {
		t.Fatal("overlay did not render the cached choice")
	}
	(<-s.jobs)(s.ctx, session)
	if tr.open || session.RemoteActive() || !s.menuWatch {
		t.Fatal("showing the cached page opened GSX or left number selection disabled")
	}
}

// A stale click waits for the simulator and ignores the old queued redraw
func TestShowMenuRequestsFreshPageAtCutoff(t *testing.T) {
	s, cache := testMenuState(t)
	page := gsx.ParseMenu("Ramp\nJetway")
	s.rememberMenu(cache, page, time.Now().Add(-15*time.Second), true)
	s.toggleMenu()
	s.main.FrameFn()
	if !s.overlay || len(s.menu.Options) != 0 {
		t.Fatalf("stale choices appeared while waiting for refresh: %+v", s.menu)
	}
	tr := &menuChoices{}
	session := gsx.New(tr, "")
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	session.ReadMenu = func() gsx.Menu {
		if tr.open {
			return page
		}
		return gsx.Menu{}
	}
	s.observeMenuReads(session)
	(<-s.jobs)(s.ctx, session)
	s.main.FrameFn()
	if !tr.open || !s.menu.Same(page) {
		t.Fatalf("fresh read of unchanged choices did not reach overlay: %+v", s.menu)
	}
}

// Simulate a queued refresh while automation and an old session both have reads
func TestShowMenuReusesAutomationReadAndRejectsRetiredSession(t *testing.T) {
	s, retired := testMenuState(t)
	retiredPage := gsx.ParseMenu("Old\nJetway")
	s.rememberMenu(retired, retiredPage, time.Now(), true)
	current := &sessionMenuCache{}
	s.menuCache.Store(current)
	s.toggleMenu()
	s.main.FrameFn()
	if !s.overlay || len(s.menu.Options) != 0 {
		t.Fatalf("retired session repainted the overlay: %+v", s.menu)
	}

	tr := &menuChoices{}
	session := gsx.New(tr, "")
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	page := gsx.ParseMenu("Automation\nChoose operator")
	session.ReadMenu = func() gsx.Menu { return page }
	s.observeMenuReads(session)
	if _, err := session.OpenMenuPage(s.ctx); err != nil {
		t.Fatal(err)
	}
	// The request can use the page read during automation without opening GSX again.
	session.Sleep = func(context.Context, time.Duration) error {
		t.Fatal("showing the automation page wrote to GSX")
		return nil
	}
	(<-s.jobs)(s.ctx, session)
	s.main.FrameFn()
	if !s.menu.Same(page) {
		t.Fatalf("automation page missing: %+v", s.menu)
	}
	s.toggleMenu()
	(<-s.jobs)(s.ctx, session)
	if !tr.open {
		t.Fatal("hiding the cache released the automation menu")
	}
}
