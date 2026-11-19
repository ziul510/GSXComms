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
	raw = br.ReplaceAllString(strings.ReplaceAll(raw, "\r", ""), "\n")
	for _, line := range strings.Split(tag.ReplaceAllString(raw, ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !menu.Active {
			menu.Title, menu.Active = line, true
			continue
		}
		item := strings.TrimSpace(numbering.ReplaceAllString(line, ""))
		if item != "" && !strings.EqualFold(item, menu.Title) {
			menu.Options = append(menu.Options, item)
		}
		if len(menu.Options) == maxMenuOptions {
			break
		}
	}
	return menu
}

type Session struct {
	Sim      Transport
	ReadMenu func() Menu
	Sleep    func(context.Context, time.Duration) error
	remote   bool
}

func New(sim Transport, panel string) *Session {
	return &Session{Sim: sim, ReadMenu: func() Menu {
		data, _ := os.ReadFile(filepath.Join(panel, "menu"))
		return ParseMenu(strings.ToValidUTF8(string(data), ""))
	}, Sleep: wait}
}

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

func (s *Session) Connected() bool    { return s.Sim != nil }
func (s *Session) RemoteActive() bool { return s.remote }

func (s *Session) get(ctx context.Context, name string) (float64, error) {
	if s.Sim == nil {
		return 0, ErrDisconnected
	}
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
	if err != nil {
		return v >= 0.5, 0, errors.Join(ErrSpeedUnavailable, err)
	}
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
		s.remote = true
		if err := s.Sleep(ctx, 100*time.Millisecond); err != nil {
			return err
		}
		if err := s.set(ctx, MenuOpen, 1); err != nil {
			return errors.Join(err, s.Release(ctx))
		}
		if err := s.Sleep(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
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
	return s.Sleep(ctx, 200*time.Millisecond)
}

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
	menuErr := s.set(ctx, MenuOpen, 0)
	_ = s.Sleep(ctx, 100*time.Millisecond)
	err := errors.Join(menuErr, s.set(ctx, Remote, 0))
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
	if !s.remote {
		return s.Sim.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := s.Release(ctx)
	return errors.Join(err, s.Sim.Close())
}

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

func (s *Session) WaitMenu(ctx context.Context) (Menu, error) {
	return s.waitMenu(ctx, nil)
}

func (s *Session) waitMenu(ctx context.Context, previous *Menu) (Menu, error) {
	var menu Menu
	for i := 0; i < 8; i++ {
		menu = s.ReadMenu()
		if len(menu.Options) > 0 && (previous == nil || menu.Title != previous.Title || !slices.Equal(menu.Options, previous.Options)) {
			return menu, nil
		}
		if err := s.Sleep(ctx, 500*time.Millisecond); err != nil {
			return menu, err
		}
	}
	if previous != nil {
		return menu, errors.New("GSX Additional Services menu did not become active")
	}
	return menu, errors.New("GSX menu opened but did not become active")
}

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
			if index := optionContaining(options, keywords); index >= 0 {
				return index
			}
			return optionContaining(options, commands.Aliases(service))
		}
		index, _ := commands.Match(text, options)
		return index
	}
	if stuck(menu.Options) {
		return "", ErrHotfix
	}
	index := find(menu.Options)
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

func (s *Session) Location(ctx context.Context) (float64, float64, error) {
	lat, err := s.get(ctx, "PLANE LATITUDE")
	if err != nil {
		return 0, 0, err
	}
	lon, err := s.get(ctx, "PLANE LONGITUDE")
	return lat, lon, err
}
