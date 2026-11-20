// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package mute

import (
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/ziul510/GSXCommsGo/internal/config"
)

type identity struct {
	Endpoint       string
	Session        string
	PID            uint32
	ProcessStarted uint64
}

type record struct {
	ID       identity
	WasMuted bool
}

type audioSession struct {
	ID    identity
	Muted bool
	Label string
	Set   func(bool) error
}

type backend interface {
	Visit(func(audioSession) error) error
	Alive(identity) bool
}

func read(path string) ([]record, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []record
	err = json.Unmarshal(raw, &records)
	return records, err
}

func write(path string, records []record) error {
	b, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteAtomic(path, b)
}

func match(label string) bool {
	label = strings.ToLower(label)
	// The GSX substring would also match the player itself.
	if strings.Contains(label, "gsxcomms") {
		return false
	}
	for _, token := range []string{"couatl", "gsx", "fsdreamteam", "fsdt", "addon manager"} {
		if strings.Contains(label, token) {
			return true
		}
	}
	return false
}

func acquire(path string, api backend) error {
	old, err := read(path)
	if err != nil {
		return err
	}
	// Restore interrupted mutes first and carry forward any sessions still missing.
	if len(old) > 0 {
		if err := restore(path, api); err != nil {
			return err
		}
		old, err = read(path)
		if err != nil {
			return err
		}
	}
	return api.Visit(func(s audioSession) error {
		if !match(s.Label) || s.Muted || s.ID.PID == 0 || s.ID.Session == "" || s.ID.Endpoint == "" || s.ID.ProcessStarted == 0 {
			return nil
		}
		// Keep the first recorded state for sessions already waiting on recovery.
		for _, entry := range old {
			if entry.ID == s.ID {
				return nil
			}
		}
		old = append(old, record{ID: s.ID, WasMuted: s.Muted})
		// Recovery needs the original state on disk before muting anything.
		if err := write(path, old); err != nil {
			return err
		}
		return s.Set(true)
	})
}

func restore(path string, api backend) error {
	records, err := read(path)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return clearJournal(path)
	}
	restored := map[identity]bool{}
	var failures error
	err = api.Visit(func(s audioSession) error {
		for _, entry := range records {
			if entry.ID == s.ID {
				// One failed restore should leave the other sessions eligible for recovery.
				if err := s.Set(entry.WasMuted); err != nil {
					failures = errors.Join(failures, err)
				} else {
					restored[s.ID] = true
				}
				break
			}
		}
		return nil
	})
	var pending []record
	// Unplugging an audio device can hide the session while its process is still alive.
	for _, entry := range records {
		if !restored[entry.ID] && api.Alive(entry.ID) {
			pending = append(pending, entry)
		}
	}
	if len(pending) == 0 {
		return errors.Join(err, failures, clearJournal(path))
	}
	return errors.Join(err, failures, write(path, pending))
}

func clearJournal(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
