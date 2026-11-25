// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	source  os.FileInfo
	readErr error
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
	Sim              Transport
	ReadMenu         func() Menu
	OnReadError      func(error)
	OnChoice         func(Menu, int)
	OnOptionMatched  func() // Start audio protection before sending the choice
	OnManualOperator func(Menu)
	BeforeChoice     func() error
	Sleep            func(context.Context, time.Duration) error
	remote           bool
	remoteOnly       bool // remote flag acquired without opening a menu
	pendingMenu      *Menu
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
		read := func() ([]byte, os.FileInfo, error) {
			// Capture metadata from the same file handle as the menu bytes.
			file, err := os.Open(path)
			if err == nil {
				defer file.Close()
				var data []byte
				data, err = io.ReadAll(file)
				if err == nil {
					var info os.FileInfo
					info, err = file.Stat()
					if err == nil {
						reported = ""
						return data, info, nil
					}
				}
			}
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil, nil
			}
			if err.Error() != reported && s.OnReadError != nil {
				reported = err.Error()
				s.OnReadError(err)
			}
			return nil, nil, err
		}
		data, source, err := read()
		if source == nil || len(data) == 0 {
			snapshot := empty
			snapshot.source, snapshot.readErr = source, err
			return snapshot
		}
		if bytes.Equal(last, data) && sameSource(cached.source, source) {
			return cached
		}
		// Couatl can replace the panel while it is still being written
		for range 2 {
			time.Sleep(10 * time.Millisecond)
			next, nextSource, err := read()
			if nextSource == nil || len(next) == 0 {
				snapshot := empty
				snapshot.source, snapshot.readErr = nextSource, err
				return snapshot
			}
			if bytes.Equal(data, next) && sameSource(source, nextSource) {
				last, cached = next, ParseMenu(strings.ToValidUTF8(string(next), ""))
				cached.source = nextSource
				return cached
			}
			data, source = next, nextSource
		}
		// Keep the latest file identity even if a write kept the content unstable.
		unstable := empty
		unstable.source = source
		return unstable
	}
	return s
}

// Compare both the file identity and its last write time to detect an identical rewrite.
func sameSource(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.ModTime().Equal(b.ModTime()) && os.SameFile(a, b)
}

// CurrentMenu hides the pre-open panel until Couatl publishes a fresh snapshot.
func (s *Session) CurrentMenu() Menu {
	menu := s.ReadMenu()
	if !s.menuIsFresh(menu) {
		return ParseMenu("")
	}
	return menu
}

// Judge a candidate against the outstanding open without rereading the file
func (s *Session) menuIsFresh(menu Menu) bool {
	// In-memory menu sources provide snapshots directly and have no file metadata.
	return s.pendingMenu == nil || len(menu.Options) > 0 &&
		!(sameSource(menu.source, s.pendingMenu.source) && (len(s.pendingMenu.Options) == 0 || menu.Same(*s.pendingMenu)))
}

func (m Menu) Same(other Menu) bool {
	return m.Title == other.Title && slices.Equal(m.Options, other.Options)
}

// SameSnapshot includes the on-disk generation when the panel has one
func (m Menu) SameSnapshot(other Menu) bool {
	return m.Same(other) && (m.source == nil && other.source == nil || sameSource(m.source, other.source))
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
		if errors.Is(err, ErrDisconnected) || errors.Is(err, ErrWriteUncertain) {
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
	return s.open(ctx, ctx, nil)
}

// Track which operation may change ownership and which page it can replace
type menuOpenRequest struct {
	readout  bool
	expected *Menu
	retry    bool
}

// Let callers stop panel waits while keeping simulator exchanges intact
func (s *Session) open(ctx, waitCtx context.Context, request *menuOpenRequest) error {
	if err := waitCtx.Err(); err != nil {
		return err
	}
	if !s.Connected() {
		return ErrDisconnected
	}
	current, err := s.get(ctx, MenuOpen)
	if err != nil {
		return err
	}
	if err := waitCtx.Err(); err != nil {
		return err
	}
	// Readout can wait on the pilot's existing page without claiming it
	readout := request != nil && request.readout
	// A retry can pulse a menu it acquired on its first attempt
	preserveOpen := readout || request != nil && request.retry && (!s.remote || s.remoteOnly)
	if preserveOpen && current >= 0.5 {
		return nil
	}
	// A remote pulse requests a new panel even if the session already owns the menu.
	previous := s.ReadMenu()
	if err := waitCtx.Err(); err != nil {
		return err
	}
	if previous.readErr != nil {
		// No request was sent, so the failed read cannot invalidate later pilot menus
		return fmt.Errorf("could not establish GSX menu freshness: %w", previous.readErr)
	}
	// The first pulse can publish during the next MENU_OPEN round trip
	if request != nil && request.retry && s.pendingMenu != nil && s.menuIsFresh(previous) {
		return nil
	}
	// Validate the exact pre-pulse snapshot used for an idle numbered choice
	if request != nil && request.expected != nil && len(previous.Options) > 0 && !request.expected.SameSnapshot(previous) {
		return ErrMenuChanged
	}
	s.pendingMenu = &previous
	var check func() error
	if request != nil && request.expected != nil {
		check = func() error {
			// Check again under the write gate before the first refresh trigger
			current := s.ReadMenu()
			if current.readErr != nil {
				return current.readErr
			}
			if len(current.Options) > 0 && !request.expected.SameSnapshot(current) {
				return ErrMenuChanged
			}
			s.pendingMenu = &current
			return nil
		}
	}
	if current < 0.5 {
		unowned := !s.remote
		if err := s.enableRemote(ctx); err != nil {
			if unowned && s.remote {
				s.remoteOnly = true
			}
			return err
		}
		if unowned {
			s.remoteOnly = true
		}
		if err := s.Sleep(waitCtx, 100*time.Millisecond); err != nil {
			return err
		}
		if preserveOpen {
			// The pilot can open the menu while the remote flag is being set
			current, err := s.get(ctx, MenuOpen)
			if err != nil {
				return err
			}
			if current >= 0.5 && unowned && !readout {
				// Recovery only acquired a flag, so return it to the pilot
				return s.ReleaseOwned(ctx)
			}
			if current >= 0.5 || waitCtx.Err() != nil {
				return waitCtx.Err()
			}
		}
		// The closed menu needs the remote flag set before the open request
		openErr := s.write(ctx, MenuOpen, 1, check)
		if openErr == nil || errors.Is(openErr, ErrWriteUncertain) {
			s.remoteOnly = false
		}
		if openErr != nil {
			if readout {
				return openErr
			}
			return errors.Join(openErr, s.Release(ctx))
		}
		if err := s.Sleep(waitCtx, 100*time.Millisecond); err != nil {
			return err
		}
		// The open write was already the first refresh trigger
		check = nil
	}
	// Pulse the remote flag for both a fresh open and an existing menu
	if err := s.write(ctx, Remote, 0, check); err != nil {
		return err
	}
	if err := s.Sleep(waitCtx, 100*time.Millisecond); err != nil {
		return err
	}
	if err := s.enableRemote(ctx); err != nil {
		return err
	}
	s.remoteOnly = false
	// Let GSX apply remote control before callers read or select from the menu
	return s.Sleep(waitCtx, 200*time.Millisecond)
}

// Track possible remote ownership so idle and shutdown cleanup release it.
func (s *Session) enableRemote(ctx context.Context) error {
	err := s.set(ctx, Remote, 1)
	if err == nil || errors.Is(err, ErrWriteUncertain) {
		s.remote = true
	}
	return err
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
	if s.remoteOnly {
		// This session may release its flag, but it never opened the menu
		err := s.set(ctx, Remote, 0)
		s.remote = err != nil && s.Connected()
		s.remoteOnly = s.remote
		return err
	}
	// Close the menu first, then attempt to release remote control even if closing failed
	menuErr := s.set(ctx, MenuOpen, 0)
	_ = s.Sleep(ctx, 100*time.Millisecond)
	err := errors.Join(menuErr, s.set(ctx, Remote, 0))
	// Keep tracking remote control after a failed release so idle cleanup can retry
	s.remote = err != nil && s.Connected()
	// A confirmed close leaves only the flag to release on the next attempt
	s.remoteOnly = s.remote && menuErr == nil
	return err
}

// Allow two write confirmations plus transport contention or a retry.
const releaseTimeout = 2 * time.Second

// Release only the flag when the live menu has already closed
func (s *Session) ReleaseClosedMenu(ctx context.Context) error {
	// A later menu may belong to the pilot
	s.remoteOnly = s.remote
	return s.ReleaseOwned(ctx)
}

// ReleaseOwned gives a canceled operation time to relinquish its menu
func (s *Session) ReleaseOwned(ctx context.Context) error {
	if !s.RemoteActive() {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	return s.Release(cleanup)
}

func (s *Session) Close() error {
	if s.Sim == nil {
		return nil
	}
	defer func() {
		s.Sim = nil
		s.remote = false
		s.remoteOnly = false
		s.pendingMenu = nil
	}()
	// A monitoring session should leave a menu opened by the simulator alone
	if !s.remote {
		return s.Sim.Close()
	}
	// Shutdown can cancel the caller before the menu has been released
	err := s.ReleaseOwned(context.Background())
	return errors.Join(err, s.Sim.Close())
}

// Let GSX process the slot before the next choice
func (s *Session) Choose(ctx context.Context, index int) error {
	page := s.CurrentMenu()
	// Deny raw console choices while a newly opened panel is still stale.
	if s.pendingMenu != nil && len(page.Options) == 0 {
		return ErrMenuInactive
	}
	_, err := s.choose(ctx, page, index, nil)
	return err
}

// ChooseOn checks the page again because a GSX slot can change while a command waits
func (s *Session) ChooseOn(ctx context.Context, page Menu, index int) error {
	_, err := s.chooseOn(ctx, page, index, s.OnOptionMatched)
	return err
}

// Navigation can use the same checks without triggering command side effects
func (s *Session) chooseOn(ctx context.Context, page Menu, index int, onMatch func()) (bool, error) {
	if index < 0 || index >= len(page.Options) {
		return false, ErrMenuChanged
	}
	return s.choose(ctx, page, index, func() error {
		if !page.SameSnapshot(s.CurrentMenu()) {
			return ErrMenuChanged
		}
		if s.BeforeChoice != nil {
			if err := s.BeforeChoice(); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Start protection before Couatl can speak, once even if the write retries
		if onMatch != nil {
			onMatch()
			onMatch = nil
		}
		return nil
	})
}

// Report whether GSX could have applied the choice even if settling fails.
func (s *Session) choose(ctx context.Context, page Menu, index int, check func() error) (bool, error) {
	if index < 0 || index >= maxMenuOptions {
		return false, errors.New("invalid GSX item")
	}
	err := s.write(ctx, MenuChoice, float64(index), check)
	if err != nil && !errors.Is(err, ErrWriteUncertain) {
		// GSX or the pilot can change the panel independently of this write.
		return false, err
	}
	// A lost reply can still mean Couatl acted, but the link cannot verify it
	if errors.Is(err, ErrDisconnected) {
		return true, err
	}
	s.remote = true
	s.remoteOnly = false
	// Only highlight a successful choice when the snapshot still contains that slot.
	if err == nil && s.OnChoice != nil && index < len(page.Options) {
		s.OnChoice(page, index)
	}
	// Give GSX time to apply the choice before the next operation reads the panel.
	settleErr := s.Sleep(ctx, 300*time.Millisecond)
	if settleErr != nil && err != nil {
		return true, errors.Join(err, settleErr)
	}
	if err != nil {
		return true, err
	}
	return true, settleErr
}

// OpenMenuPage makes a second request if Couatl accepted the open but never supplied choices
func (s *Session) OpenMenuPage(ctx context.Context) (Menu, error) {
	return s.openMenuPage(ctx, ctx, nil)
}

// RetryMenuPage keeps the outstanding baseline through the next simulator read
func (s *Session) RetryMenuPage(ctx context.Context) (Menu, error) {
	// Background recovery has no authority to replace an open pilot menu
	return s.openMenuPage(ctx, ctx, &menuOpenRequest{retry: true})
}

// ReopenMenuPage checks the displayed generation before sending a refresh pulse
func (s *Session) ReopenMenuPage(ctx context.Context, expected Menu) (Menu, error) {
	return s.openMenuPage(ctx, ctx, &menuOpenRequest{expected: &expected})
}

// Stop panel waits without canceling a request already sent to the simulator
func (s *Session) openMenuPage(ctx, waitCtx context.Context, request *menuOpenRequest) (Menu, error) {
	if request == nil {
		request = &menuOpenRequest{}
	}
	var menu Menu
	var err error
	for range 2 {
		if err = s.open(ctx, waitCtx, request); err != nil {
			break
		}
		// The caller's own first pulse can change the baseline for any retry
		request.expected = nil
		request.retry = true
		if menu, err = s.WaitMenu(waitCtx); err == nil || waitCtx.Err() != nil || ctx.Err() != nil {
			break
		}
	}
	return menu, err
}

// ReadMenuPage reuses an open menu and only releases ownership acquired for readout
func (s *Session) ReadMenuPage(ctx, waitCtx context.Context) (Menu, error) {
	if err := waitCtx.Err(); err != nil {
		return Menu{}, err
	}
	owned := s.remote && !s.remoteOnly
	request := &menuOpenRequest{readout: true}
	menu, err := s.openMenuPage(ctx, waitCtx, request)
	if !owned {
		// Release a temporary menu even when Settings cancels the panel wait
		err = errors.Join(err, s.ReleaseOwned(ctx))
	}
	return menu, err
}

// RefreshAndRelease takes a new page snapshot before handing the menu back to GSX
func (s *Session) RefreshAndRelease(ctx context.Context) (Menu, error) {
	// Flag-only recovery must not reopen and take over a pilot-owned page
	if s.remoteOnly {
		return s.CurrentMenu(), s.Release(ctx)
	}
	s.MarkMenuStale()
	if err := s.Release(ctx); err != nil {
		return Menu{}, err
	}
	menu, err := s.OpenMenuPage(ctx)
	return menu, errors.Join(err, s.ReleaseOwned(ctx))
}

// Hide the last known panel after the simulator closes it
func (s *Session) MarkMenuStale() {
	// Partial release can make the current panel stale before the next open
	if s.pendingMenu == nil {
		previous := s.ReadMenu()
		// The overlay retry suppresses rows while a locked file is unreadable
		if previous.readErr == nil {
			s.pendingMenu = &previous
		}
	}
}

// MenuOpened reads the live flag independently of the asynchronously updated file
func (s *Session) MenuOpened(ctx context.Context) (bool, error) {
	value, err := s.get(ctx, MenuOpen)
	return value >= 0.5, err
}

// WaitMenu confirms the panel has updated after a new open.
func (s *Session) WaitMenu(ctx context.Context) (Menu, error) {
	return s.waitMenu(ctx, nil, nil)
}

// TakeRefreshedMenu accepts a late publication before another pulse can hide it
func (s *Session) TakeRefreshedMenu() Menu {
	if s.pendingMenu == nil {
		return Menu{}
	}
	menu := s.CurrentMenu()
	if len(menu.Options) > 0 {
		s.pendingMenu = nil
	}
	return menu
}

func (s *Session) waitMenu(ctx context.Context, previous *Menu, accept func(Menu) bool) (Menu, error) {
	var menu Menu
	// Read once after the final wait before the caller starts another open
	for i := 0; i <= 8; i++ {
		if err := ctx.Err(); err != nil {
			return Menu{}, err
		}
		menu = s.CurrentMenu()
		// The panel file can still show the parent menu after a submenu choice
		if len(menu.Options) > 0 && (previous == nil || !menu.Same(*previous)) && (accept == nil || accept(menu)) {
			s.pendingMenu = nil
			return menu, nil
		}
		if i == 8 {
			break
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
	menu := s.CurrentMenu()
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
			// GSX exposes one jetway toggle and no jetway state API, so both verbs select the same item.
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
		_, navigationErr := s.chooseOn(ctx, menu, extra, nil)
		if navigationErr != nil && !canCheckChoice(ctx, navigationErr) {
			return "", navigationErr
		}
		if err := s.Sleep(ctx, 500*time.Millisecond); err != nil {
			return "", errors.Join(navigationErr, err)
		}
		var accept func(Menu) bool
		if navigationErr != nil {
			// An intermediate page cannot corroborate the consumed navigation slot
			accept = func(page Menu) bool { return strings.Contains(menuLabel(page.Title), "additional services") }
		}
		var err error
		menu, err = s.waitMenu(ctx, &menu, accept)
		if err != nil {
			_ = s.Release(ctx)
			return "", errors.Join(navigationErr, err)
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
			var backErr error
			if back >= 0 {
				_, backErr = s.chooseOn(ctx, menu, back, nil)
			}
			if back < 0 || backErr != nil && !canCheckChoice(ctx, backErr) {
				_ = s.Release(ctx)
			} else if _, err := s.waitMenu(ctx, &menu, parent.Same); err != nil {
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
	sent, choiceErr := s.chooseOn(ctx, menu, index, s.OnOptionMatched)
	var selected string
	// A confirmed service survives a later settling or operator error
	if sent && !errors.Is(choiceErr, ErrWriteUncertain) {
		selected = menu.Options[index]
	}
	// An uncertain choice can still open the operator page without a replay
	if choiceErr != nil && (!sent || !canCheckChoice(ctx, choiceErr)) {
		return selected, choiceErr
	}
	// Only a changed operator page can trigger an automatic selection
	if autoOperator || choiceErr != nil || s.OnManualOperator != nil {
		for attempt := range 7 {
			// The service choice already allowed GSX time to publish its next page
			if attempt > 0 {
				if err := s.Sleep(ctx, 500*time.Millisecond); err != nil {
					return selected, errors.Join(choiceErr, err)
				}
			}
			page := s.ReadMenu()
			// GSX can briefly leave the old page in place or clear it while opening the next page.
			if page.Same(menu) || len(page.Options) == 0 {
				continue
			}
			i := optionContaining(page.Options, []string{"gsx choice"})
			title := " " + strings.Join(commands.Tokens(page.Title), " ") + " "
			operator := i >= 0 || strings.Contains(title, " operator ") || strings.Contains(title, " operators ")
			// Require the expected page before interpreting an uncertain service
			if (choiceErr != nil || !autoOperator) && !operator {
				continue
			}
			if !autoOperator {
				if s.OnManualOperator != nil {
					s.OnManualOperator(page)
				}
				return menu.Options[index], nil
			}
			if i >= 0 {
				return menu.Options[index], s.ChooseOn(ctx, page, i)
			}
			// A service can open a follow-up with a title that never says operator
			if operator || service != "" {
				// Leave any page that replaced the inspected snapshot alone
				if !page.SameSnapshot(s.CurrentMenu()) {
					return menu.Options[index], ErrMenuChanged
				}
				return menu.Options[index], s.Release(ctx)
			}
		}
	}

	return selected, choiceErr
}

// Only a possibly applied write on a live link can be reconciled by reading the panel
func canCheckChoice(ctx context.Context, err error) bool {
	return errors.Is(err, ErrWriteUncertain) && !errors.Is(err, ErrDisconnected) && ctx.Err() == nil
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
