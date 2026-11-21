// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"bytes"
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
	"unicode"

	"github.com/ziul510/GSXCommsGo/internal/commands"
	"github.com/ziul510/GSXCommsGo/internal/config"
)

const (
	Started    = "L:FSDT_GSX_COUATL_STARTED"
	Remote     = "L:FSDT_GSX_SET_REMOTECONTROL"
	MenuOpen   = "L:FSDT_GSX_MENU_OPEN"
	MenuChoice = "L:FSDT_GSX_MENU_CHOICE"
	Speed      = "GPS GROUND SPEED"

	maxMenuOptions = 9
)

var (
	ErrDisconnected           = errors.New("SimConnect handshake required")
	ErrSpeedUnavailable       = errors.New("ground speed unavailable")
	ErrMenuChanged            = errors.New("GSX menu changed before selection")
	ErrHotfix                 = errors.New("Please select the second option in the GSX menu, apply the hotfixes, and restart GSXComms")
	ErrMenuInactive           = errors.New("GSX menu opened but did not become active")
	ErrAdditionalMenuInactive = errors.New("GSX Additional Services menu did not become active")
	ErrNoMenuMatch            = errors.New("No matching GSX menu option found")
)

type serviceMatchError string

func (err serviceMatchError) Error() string { return fmt.Sprintf("service %q not found", string(err)) }
func (err serviceMatchError) Unwrap() error { return ErrNoMenuMatch }

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
	numbering = regexp.MustCompile(`^(?:\[\d+\]\s*|\d+[).:-](?:\s+|$)|[1-9][).:](\pL))`)
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
		item := strings.TrimSpace(numbering.ReplaceAllString(line, "${1}"))
		// Skip repeated titles so they do not take up selectable slots.
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
	Sim          Transport
	ReadMenu     func() Menu
	OnReadError  func(error)
	OnChoice     func(Menu, int)
	BeforeChoice func() error
	Sleep        func(context.Context, time.Duration) error
	remote       bool
}

// The app can create a session before connecting and reuse it as transports come and go
func New(sim Transport, panel string) *Session {
	s := &Session{Sim: sim, Sleep: wait}
	path := filepath.Join(panel, "menu")
	empty := ParseMenu("")
	var last []byte
	var cached Menu
	var reported string
	s.ReadMenu = func() Menu {
		read := func() ([]byte, bool) {
			data, err := os.ReadFile(path)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) && err.Error() != reported && s.OnReadError != nil {
					reported = err.Error()
					s.OnReadError(err)
				}
				return nil, false
			}
			reported = ""
			return data, true
		}
		data, ok := read()
		if !ok || len(data) == 0 {
			return empty
		}
		if bytes.Equal(last, data) {
			return cached
		}
		// Couatl can replace the panel while it is still being written
		for range 2 {
			time.Sleep(10 * time.Millisecond)
			next, ok := read()
			if !ok || len(next) == 0 {
				return empty
			}
			if bytes.Equal(data, next) {
				last, cached = next, ParseMenu(strings.ToValidUTF8(string(next), ""))
				return cached
			}
			data = next
		}
		return empty
	}
	return s
}

func (m Menu) Same(other Menu) bool {
	return m.Title == other.Title && slices.Equal(m.Options, other.Options)
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
		if errors.Is(err, ErrDisconnected) {
			return 0, err
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
	return s.write(ctx, name, value, nil)
}

func (s *Session) write(ctx context.Context, name string, value float64, check func() error) error {
	if s.Sim == nil {
		return ErrDisconnected
	}
	// Repeat failed writes a few times before returning the last error to the caller
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		switch {
		case check == nil:
			err = s.Sim.Set(ctx, name, value)
		default:
			var checkErr error
			validate := func() error {
				checkErr = check()
				return checkErr
			}
			if transport, ok := s.Sim.(interface {
				setChecked(context.Context, string, float64, func() error) error
			}); ok {
				err = transport.setChecked(ctx, name, value, validate)
			} else if validate() == nil {
				err = s.Sim.Set(ctx, name, value)
			}
			if checkErr != nil {
				return checkErr
			}
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrDisconnected) || errors.Is(err, errWriteUncertain) {
			return err
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
		if errors.Is(err, ErrDisconnected) {
			return false, 0, err
		}
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

// Let GSX process the slot before the next choice
func (s *Session) Choose(ctx context.Context, index int) error {
	return s.choose(ctx, s.ReadMenu(), index, nil)
}

// ChooseOn checks the page again because a GSX slot can change while a command waits
func (s *Session) ChooseOn(ctx context.Context, page Menu, index int) error {
	if index < 0 || index >= len(page.Options) {
		return ErrMenuChanged
	}
	return s.choose(ctx, page, index, func() error {
		if !page.Same(s.ReadMenu()) {
			return ErrMenuChanged
		}
		if s.BeforeChoice != nil {
			return s.BeforeChoice()
		}
		return nil
	})
}

func (s *Session) choose(ctx context.Context, page Menu, index int, check func() error) error {
	if index < 0 || index >= maxMenuOptions {
		return errors.New("invalid GSX item")
	}
	if err := s.write(ctx, MenuChoice, float64(index), check); err != nil {
		return err
	}
	s.remote = true
	// Only highlight a successful choice when the snapshot still contains that slot.
	if s.OnChoice != nil && index < len(page.Options) {
		s.OnChoice(page, index)
	}
	// Give GSX time to apply the choice before the next operation reads the panel.
	return s.Sleep(ctx, 300*time.Millisecond)
}

// OpenMenuPage makes a second request if Couatl accepted the open but never supplied choices
func (s *Session) OpenMenuPage(ctx context.Context) (Menu, error) {
	var menu Menu
	var err error
	for range 2 {
		if err = s.Open(ctx); err != nil {
			break
		}
		if menu, err = s.WaitMenu(ctx); err == nil || ctx.Err() != nil {
			break
		}
	}
	return menu, err
}

// RefreshAndRelease takes a new page snapshot before handing the menu back to GSX
func (s *Session) RefreshAndRelease(ctx context.Context) (Menu, error) {
	if err := s.Release(ctx); err != nil {
		return Menu{}, err
	}
	menu, err := s.OpenMenuPage(ctx)
	if s.RemoteActive() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		err = errors.Join(err, s.Release(closeCtx))
	}
	return menu, err
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
		if len(menu.Options) > 0 && (previous == nil || !menu.Same(*previous)) {
			return menu, nil
		}
		if err := s.Sleep(ctx, 500*time.Millisecond); err != nil {
			return menu, err
		}
	}
	// Distinguish a delayed submenu from a menu that never populated
	if previous != nil {
		return menu, ErrAdditionalMenuInactive
	}
	return menu, ErrMenuInactive
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

func menuLabel(text string) string {
	return strings.ToLower(strings.TrimRightFunc(strings.TrimSpace(text), func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSpace(r)
	}))
}

func stuck(options []string) bool {
	if len(options) != 2 {
		return false
	}
	a, b := menuLabel(options[0]), menuLabel(options[1])
	navigation := []string{"back", "cancel", "close"}
	// The broken page has two different navigation choices and no service choices.
	return a != b && slices.Contains(navigation, a) && slices.Contains(navigation, b)
}

func (s *Session) Execute(ctx context.Context, text, service string, keywords []string, autoOperator bool) (string, error) {
	if !s.Connected() {
		return "", ErrDisconnected
	}
	menu := s.ReadMenu()
	// Start services by opening the menu A text choice can use the current page
	if service != "" || !s.RemoteActive() || len(menu.Options) == 0 {
		var err error
		menu, err = s.OpenMenuPage(ctx)
		if err != nil {
			return "", err
		}
	}
	find := func(options []string) int {
		if service != "" {
			if strings.HasPrefix(service, config.CustomServicePrefix) && len(keywords) == 1 {
				if index := slices.IndexFunc(options, func(option string) bool {
					return strings.EqualFold(option, keywords[0])
				}); index >= 0 {
					return index
				}
			}
			return optionContaining(options, keywords)
		}
		// Without a service ID, score the transcript against the visible labels
		index, _ := commands.Match(text, options)
		return index
	}
	if stuck(menu.Options) {
		return "", ErrHotfix
	}
	index := find(menu.Options)
	if service == "" && index < 0 {
		// A number refers to the displayed page, so a missing slot cannot trigger a submenu search.
		if _, explicit := commands.ExplicitNumber(text); explicit {
			return "", ErrNoMenuMatch
		}
	}
	// Search the submenu only after the current page has no match
	if extra := optionContaining(menu.Options, []string{"additional services"}); index < 0 && extra >= 0 {
		parent := menu
		if err := s.ChooseOn(ctx, menu, extra); err != nil {
			return "", err
		}
		if err := s.Sleep(ctx, 500*time.Millisecond); err != nil {
			return "", err
		}
		var err error
		menu, err = s.waitMenu(ctx, &menu)
		if err != nil {
			_ = s.Release(ctx)
			return "", err
		}
		if stuck(menu.Options) {
			return "", ErrHotfix
		}
		index = find(menu.Options)
		// Undo submenu navigation if it didn't turn up a matching choice
		if index < 0 {
			back := slices.IndexFunc(menu.Options, func(option string) bool {
				return slices.Contains([]string{"back", "close", "cancel"}, menuLabel(option))
			})
			if back < 0 || s.ChooseOn(ctx, menu, back) != nil {
				_ = s.Release(ctx)
			} else if page, err := s.waitMenu(ctx, &menu); err != nil || !page.Same(parent) {
				_ = s.Release(ctx)
			}
		}
	}
	if index < 0 {
		if service != "" {
			return "", serviceMatchError(service)
		}
		return "", ErrNoMenuMatch
	}
	selected := menu.Options[index]
	if err := s.ChooseOn(ctx, menu, index); err != nil {
		return "", err
	}
	// Only a changed operator page can trigger an automatic selection
	if autoOperator {
		for range 6 {
			if err := s.Sleep(ctx, 500*time.Millisecond); err != nil {
				return selected, err
			}
			page := s.ReadMenu()
			// GSX can briefly leave the old page in place or clear it while opening the next page.
			if page.Same(menu) || len(page.Options) == 0 {
				continue
			}
			i := slices.IndexFunc(page.Options, func(label string) bool { return menuLabel(label) == "gsx choice" })
			if i >= 0 {
				return selected, s.ChooseOn(ctx, page, i)
			}
			title := " " + strings.Join(commands.Tokens(page.Title), " ") + " "
			if strings.Contains(title, " operator ") || strings.Contains(title, " operators ") {
				return selected, s.dismissOperator(ctx, page)
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

func (s *Session) Altitude(ctx context.Context) (float64, error) {
	return s.get(ctx, "PLANE ALTITUDE")
}

// Check the menu flag before falling back to the first row
func (s *Session) dismissOperator(ctx context.Context, page Menu) error {
	if !page.Same(s.ReadMenu()) {
		return ErrMenuChanged
	}
	err := s.Release(ctx)
	if errors.Is(err, errWriteUncertain) || errors.Is(err, ErrDisconnected) {
		return err
	}
	if waitErr := s.Sleep(ctx, 200*time.Millisecond); waitErr != nil {
		return errors.Join(err, waitErr)
	}
	open, readErr := s.get(ctx, MenuOpen)
	if readErr != nil || open < 0.5 {
		return errors.Join(err, readErr)
	}
	if !page.Same(s.ReadMenu()) {
		return ErrMenuChanged
	}
	if enableErr := s.set(ctx, Remote, 1); enableErr != nil {
		return errors.Join(err, enableErr)
	}
	s.remote = true
	return s.ChooseOn(ctx, page, 0)
}
