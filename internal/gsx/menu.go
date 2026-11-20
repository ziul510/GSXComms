// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ziul510/GSXCommsGo/internal/commands"
)

const (
	Started    = "L:FSDT_GSX_COUATL_STARTED"
	Remote     = "L:FSDT_GSX_SET_REMOTECONTROL"
	MenuOpen   = "L:FSDT_GSX_MENU_OPEN"
	MenuChoice = "L:FSDT_GSX_MENU_CHOICE"
	Speed      = "GROUND VELOCITY"

	maxMenuOptions = 9
)

var (
	ErrDisconnected     = errors.New("SimConnect handshake required")
	ErrSpeedUnavailable = errors.New("ground speed unavailable")
	ErrHotfix           = errors.New("Please select the second option in the GSX menu, apply the hotfixes, and restart GSXComms")
)

type Transport interface {
	Get(context.Context, string) (float64, error)
	Set(context.Context, string, float64) error
	Close() error
}

type Menu struct {
	Title   string   `json:"title"`
	Options []string `json:"options"`
	Active  bool     `json:"active"`
}

var (
	br        = regexp.MustCompile(`(?i)<br\s*/?>`)
	tag       = regexp.MustCompile(`<[^>]+>`)
	numbering = regexp.MustCompile(`^\s*(?:\[?\d+\]?|\d+)[\]\).:-]?\s*`)
)

func ParseMenu(raw string) Menu {
	menu := Menu{Title: "GSX Menu", Options: []string{}}
	// Translate HTML breaks before dropping tags so each option stays on its own line
	raw = br.ReplaceAllString(strings.ReplaceAll(raw, "\r", ""), "\n")
	for _, line := range strings.Split(tag.ReplaceAllString(raw, ""), "\n") {
		line = strings.TrimSpace(line)
		// Ignore panel annotations and empty lines before looking for the title
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// The first visible line is the title, which can appear before any choices
		if !menu.Active {
			menu.Title, menu.Active = line, true
			continue
		}
		// Use panel order for selection indices since labels can already carry numbers
		item := strings.TrimSpace(numbering.ReplaceAllString(line, ""))
		if item != "" && !strings.EqualFold(item, menu.Title) {
			menu.Options = append(menu.Options, item)
		}
		// Keep only the slots the GSX protocol can address
		if len(menu.Options) == maxMenuOptions {
			break
		}
	}
	return menu
}

// The transport carries simulator variables The panel file carries the menu text
type Session struct {
	Sim      Transport
	ReadMenu func() Menu
	Sleep    func(context.Context, time.Duration) error
	remote   bool
}

// The app can create a session before connecting and reuse it as transports come and go
func New(sim Transport, panel string) *Session {
	return &Session{Sim: sim, ReadMenu: func() Menu {
		// The panel file can be absent between menus Treat that as an empty menu
		data, _ := os.ReadFile(filepath.Join(panel, "menu"))
		return ParseMenu(strings.ToValidUTF8(string(data), ""))
	}, Sleep: wait}
}

// Protocol delays should stop promptly when the app shuts down
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// This reports a live transport Couatl readiness comes from Status
func (s *Session) Connected() bool    { return s.Sim != nil }
func (s *Session) RemoteActive() bool { return s.remote } // Cleanup uses this flag to tell whether GSX may still be under remote control

func (s *Session) get(ctx context.Context, name string) (float64, error) {
	if s.Sim == nil {
		return 0, ErrDisconnected
	}
	// Allow four read attempts with a short pause between failures, within the caller's deadline
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		var value float64
		value, err = s.Sim.Get(ctx, name)
		if err == nil {
			return value, nil
		}
		if attempt < 3 {
			if err := s.Sleep(ctx, 80*time.Millisecond); err != nil {
				return 0, err
			}
		}
	}
	return 0, err
}

func (s *Session) set(ctx context.Context, name string, value float64) error {
	if s.Sim == nil {
		return ErrDisconnected
	}
	// Repeat failed writes a few times before returning the last error to the caller
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = s.Sim.Set(ctx, name, value)
		if err == nil {
			return nil
		}
		if attempt < 2 {
			if err := s.Sleep(ctx, 80*time.Millisecond); err != nil {
				return err
			}
		}
	}
	return err
}

func (s *Session) Status(ctx context.Context) (ready bool, speed float64, err error) {
	v, err := s.get(ctx, Started)
	if err != nil {
		return false, 0, err
	}
	speed, err = s.get(ctx, Speed)
	// A failed speed read blocks transmission but can still report GSX readiness
	if err != nil {
		return v >= 0.5, 0, errors.Join(ErrSpeedUnavailable, err)
	}
	// Only a finite speed can pass the transmission gate
	if math.IsNaN(speed) || math.IsInf(speed, 0) {
		return v >= 0.5, 0, ErrSpeedUnavailable
	}
	return v >= 0.5, speed, nil
}

func (s *Session) Open(ctx context.Context) error {
	if !s.Connected() {
		return ErrDisconnected
	}
	current, err := s.get(ctx, MenuOpen)
	if err != nil {
		return err
	}
	if current < 0.5 {
		if err := s.set(ctx, Remote, 1); err != nil {
			return err
		}
		// Mark the remote flag before waiting so a canceled open can still be cleaned up
		s.remote = true
		if err := s.Sleep(ctx, 100*time.Millisecond); err != nil {
			return err
		}
		// The closed menu needs the remote flag set before the open request
		if err := s.set(ctx, MenuOpen, 1); err != nil {
			return errors.Join(err, s.Release(ctx))
		}
		if err := s.Sleep(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	// Pulse the remote flag for both a fresh open and an existing menu
	if err := s.set(ctx, Remote, 0); err != nil {
		return err
	}
	if err := s.Sleep(ctx, 100*time.Millisecond); err != nil {
		return err
	}
	if err := s.set(ctx, Remote, 1); err != nil {
		return err
	}
	s.remote = true
	// Let GSX apply remote control before callers read or select from the menu
	return s.Sleep(ctx, 200*time.Millisecond)
}

// Use the simulator's menu flag so the console can toggle a menu opened elsewhere
func (s *Session) Toggle(ctx context.Context) error {
	v, err := s.get(ctx, MenuOpen)
	if err != nil {
		return err
	}
	if v >= 0.5 {
		return s.Release(ctx)
	}
	return s.Open(ctx)
}

func (s *Session) Release(ctx context.Context) error {
	// Close the menu first, then attempt to release remote control even if closing failed
	menuErr := s.set(ctx, MenuOpen, 0)
	_ = s.Sleep(ctx, 100*time.Millisecond)
	err := errors.Join(menuErr, s.set(ctx, Remote, 0))
	// Keep tracking remote control after a failed release so idle cleanup can retry
	s.remote = err != nil && s.Connected()
	return err
}

func (s *Session) Close() error {
	if s.Sim == nil {
		return nil
	}
	defer func() {
		s.Sim = nil
		s.remote = false
	}()
	// A monitoring session should leave a menu opened by the simulator alone
	if !s.remote {
		return s.Sim.Close()
	}
	// Shutdown can cancel the caller before the menu has been released
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := s.Release(ctx)
	return errors.Join(err, s.Sim.Close())
}

// GSX expects a zero-based slot Give the panel time to process the choice
func (s *Session) Choose(ctx context.Context, index int) error {
	if index < 0 || index >= maxMenuOptions {
		return errors.New("invalid GSX item")
	}
	if err := s.set(ctx, MenuChoice, float64(index)); err != nil {
		return err
	}
	s.remote = true
	return s.Sleep(ctx, 300*time.Millisecond)
}

// A fresh open can accept any populated menu
func (s *Session) WaitMenu(ctx context.Context) (Menu, error) {
	return s.waitMenu(ctx, nil)
}

func (s *Session) waitMenu(ctx context.Context, previous *Menu) (Menu, error) {
	var menu Menu
	for i := 0; i < 8; i++ {
		menu = s.ReadMenu()
		// The panel file can still show the parent menu after a submenu choice
		if len(menu.Options) > 0 && (previous == nil || menu.Title != previous.Title || !slices.Equal(menu.Options, previous.Options)) {
			return menu, nil
		}
		if err := s.Sleep(ctx, 500*time.Millisecond); err != nil {
			return menu, err
		}
	}
	// Distinguish a delayed submenu from a menu that never populated
	if previous != nil {
		return menu, errors.New("GSX Additional Services menu did not become active")
	}
	return menu, errors.New("GSX menu opened but did not become active")
}

// Use menu order to break ties when several labels contain a service term
func optionContaining(options []string, keywords []string) int {
	for i, option := range options {
		for _, word := range keywords {
			if strings.Contains(strings.ToLower(option), strings.ToLower(word)) {
				return i
			}
		}
	}
	return -1
}

// The hotfix check looks for two different navigation-only choices
func stuck(options []string) bool {
	if len(options) != 2 || strings.EqualFold(options[0], options[1]) {
		return false
	}
	valid := []string{"back", "cancel", "close"}
	return slices.Contains(valid, strings.ToLower(options[0])) && slices.Contains(valid, strings.ToLower(options[1]))
}

func (s *Session) Execute(ctx context.Context, text, service string, keywords []string, autoOperator bool) (string, error) {
	if !s.Connected() {
		return "", ErrDisconnected
	}
	menu := s.ReadMenu()
	// Start services by opening the menu A text choice can use the current page
	if service != "" || !menu.Active || len(menu.Options) == 0 {
		if err := s.Open(ctx); err != nil {
			return "", err
		}
		var err error
		menu, err = s.WaitMenu(ctx)
		if err != nil {
			return "", err
		}
	}
	find := func(options []string) int {
		if service != "" {
			// Give user vocabulary precedence over the built-in service aliases
			if index := optionContaining(options, keywords); index >= 0 {
				return index
			}
			return optionContaining(options, commands.Aliases(service))
		}
		// Without a service ID, score the transcript against the visible labels
		index, _ := commands.Match(text, options)
		return index
	}
	if stuck(menu.Options) {
		return "", ErrHotfix
	}
	index := find(menu.Options)
	// Search the submenu only after the current page has no match
	if extra := optionContaining(menu.Options, []string{"additional services"}); index < 0 && extra >= 0 {
		if err := s.Choose(ctx, extra); err != nil {
			return "", err
		}
		if err := s.Sleep(ctx, 500*time.Millisecond); err != nil {
			return "", err
		}
		var err error
		menu, err = s.waitMenu(ctx, &menu)
		if err != nil {
			return "", err
		}
		if stuck(menu.Options) {
			return "", ErrHotfix
		}
		index = find(menu.Options)
		// Undo submenu navigation if it didn't turn up a matching choice
		if index < 0 {
			back := slices.IndexFunc(menu.Options, func(option string) bool {
				return slices.Contains([]string{"back", "close", "cancel"}, strings.ToLower(option))
			})
			if back >= 0 {
				_ = s.Choose(ctx, back)
			} else {
				_ = s.Release(ctx)
			}
		}
	}
	if index < 0 {
		if service != "" {
			return "", fmt.Errorf("service %q not found", service)
		}
		return "", errors.New("No matching GSX menu option found")
	}
	selected := menu.Options[index]
	if err := s.Choose(ctx, index); err != nil {
		return "", err
	}
	// The operator prompt can show up after the service selection returns
	if autoOperator {
		for i := 0; i < 6; i++ {
			if err := s.Sleep(ctx, 500*time.Millisecond); err != nil {
				return selected, err
			}
			options := s.ReadMenu().Options
			if i := optionContaining(options, []string{"automatic operator", "automatically choose", "gsx choice"}); i >= 0 {
				return selected, s.Choose(ctx, i)
			}
		}
	}
	return selected, nil
}

// Read the aircraft position used by the offline accent lookup
func (s *Session) Location(ctx context.Context) (float64, float64, error) {
	lat, err := s.get(ctx, "PLANE LATITUDE")
	if err != nil {
		return 0, 0, err
	}
	lon, err := s.get(ctx, "PLANE LONGITUDE")
	return lat, lon, err
}
