// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ziul510/GSXCommsGo/internal/gsx"
)

// Each connection owns its snapshots so retiring it also retires late reads
type sessionMenuCache struct {
	latest atomic.Pointer[menuSnapshot]
}

type menuSnapshot struct {
	page gsx.Menu
	at   time.Time
}

const idleMenuTimeout = 10 * time.Second

// Keep the automatic close deadline unless GSX asks for a manual fuel quantity
func (s *state) handleOperatorMenuTimeout(session *gsx.Session, menu gsx.Menu, now time.Time) bool {
	if s.operatorCloseAt.IsZero() {
		return false
	}
	// Leave fuel prompts available even when the pilot takes longer than the deadline
	if menu.IsFuelQuantity() {
		s.operatorCloseAt = time.Time{}
		return false
	}
	// The deadline is based on the confirmed click, even if GSX changes the page
	if !now.Before(s.operatorCloseAt) {
		s.releaseIdleMenu(session)
	}
	return true
}

// Close idle menus without reading another page
func (s *state) releaseIdleMenu(session *gsx.Session) {
	s.trace("GSX", "Releasing the idle menu")
	ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
	defer cancel()
	// Keep the pending close on failure so the next tick can retry
	if err := session.Release(ctx); err != nil {
		s.traceFailure("GSX", "Could not release the idle menu", err)
		return
	}
	s.menuOpened = false
	s.operatorCloseAt = time.Time{}
}

// The displayed page is reusable for less than 15 seconds after interaction
func (m *menuSnapshot) fresh(now time.Time) bool {
	return m != nil && now.Sub(m.at) < 15*time.Second
}

// Observe reads from all menu operations and keep the raw reader for idle polls
func (s *state) observeMenuReads(session *gsx.Session) func() gsx.Menu {
	read := session.ReadMenu
	session.ReadMenu = func() gsx.Menu {
		cache := s.menuCache.Load()
		page := read()
		s.rememberMenu(cache, page, time.Now(), true)
		return page
	}
	return read
}

// Keep the last usable page through temporary clears and idle release
func (s *state) rememberMenu(cache *sessionMenuCache, page gsx.Menu, now time.Time, interactive bool) {
	if cache == nil || len(page.Options) == 0 {
		return
	}
	previous := cache.latest.Load()
	same := previous != nil && page.Same(previous.page)
	// A steady panel file must not keep the menu fresh indefinitely
	if same && !interactive {
		return
	}
	cache.latest.Store(&menuSnapshot{page, now})
	// Skip redundant redraws unless a fresh read can satisfy a pending refresh
	if same && previous.fresh(now) {
		return
	}
	s.async(func() {
		// Queued updates may outlive the connection that read the menu
		if s.menuCache.Load() != cache {
			return
		}
		latest := cache.latest.Load()
		// A delayed redraw cannot restore the stale page after a refresh click
		if latest.at.Before(s.menuRefreshAfter) {
			return
		}
		s.menu, s.menuRefreshAfter = latest.page, time.Time{}
	})
}

// Display a fresh snapshot immediately and serialize simulator writes with automation
func (s *state) toggleMenu() {
	cache := s.menuCache.Load()
	if s.overlay {
		// Leave the overlay visible if the queued close couldn't be scheduled
		if s.addJob(func(ctx context.Context, session *gsx.Session) {
			if cache != s.menuCache.Load() {
				return
			}
			s.menuWatch = false
			// Hiding a cached page must not release a menu opened by command automation
			if s.menuOpened && session.RemoteActive() {
				if err := session.Release(ctx); err != nil {
					s.traceFailure("GSX", "Could not release the menu", err)
				}
			}
			s.menuOpened = false
		}) {
			s.overlay = false
		}
		return
	}
	if !s.ready || cache == nil {
		return
	}
	clicked := time.Now()
	snapshot := cache.latest.Load()
	fresh := snapshot.fresh(clicked)
	if s.addJob(func(ctx context.Context, session *gsx.Session) {
		if cache != s.menuCache.Load() {
			return
		}
		s.menuWatch = true
		// Automation may have supplied a page while this request was queued
		latest := cache.latest.Load()
		if fresh || (latest != nil && !latest.at.Before(clicked)) {
			return
		}
		s.menuActivity = time.Now()
		// Reading an open page avoids pulsing remote control on a live prompt
		if len(session.ReadOpenMenu(ctx).Options) != 0 {
			return
		}
		// An overlay that opens a new menu starts a separate idle period
		s.operatorCloseAt = time.Time{}
		menu, err := session.OpenMenuPage(ctx)
		s.menuOpened = session.RemoteActive()
		if err != nil {
			s.traceFailure("GSX", "Could not open the menu", err)
			s.async(func() { s.log("GSX", err.Error()) })
		} else {
			s.trace("GSX", fmt.Sprintf("Menu opened with %d options", len(menu.Options)))
		}
	}) {
		s.highlights, s.overlay = nil, true
		if fresh {
			s.menu, s.menuRefreshAfter = snapshot.page, time.Time{}
		} else {
			// Keep stale choices out of the overlay while the request is queued
			s.menu, s.menuRefreshAfter = gsx.Menu{}, clicked
		}
	}
}
