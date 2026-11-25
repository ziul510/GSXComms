// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package mute

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ziul510/GSXCommsGo/internal/config"
)

// Match the endpoint, session instance, and process lifetime before restoring audio
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

// A missing journal means there is no interrupted mute to recover
func read(path string) ([]record, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read mute journal: %w", err)
	}
	var records []record
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("mute journal has invalid JSON: %w", err)
	}
	return records, nil
}

// Replace the whole journal atomically so a crash can't leave half a JSON record
func write(path string, records []record) error {
	b, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	if err := config.WriteAtomic(path, b); err != nil {
		return fmt.Errorf("write mute journal: %w", err)
	}
	return nil
}

func match(label string) bool {
	label = strings.ToLower(label)
	// The GSX substring would also match the player itself
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

// Scan for matching sessions without restoring any already held by this watchdog
func acquire(path string, api backend) (bool, error) {
	old, err := read(path)
	if err != nil {
		return false, err
	}
	recorded := make(map[identity]bool, len(old))
	for _, entry := range old {
		recorded[entry.ID] = true
	}
	var matched bool
	var failures error
	err = api.Visit(func(s audioSession) error {
		if !match(s.Label) {
			return nil
		}
		// An incomplete identity cannot be restored safely
		if s.ID.PID == 0 || s.ID.Session == "" || s.ID.Endpoint == "" || s.ID.ProcessStarted == 0 {
			failures = errors.Join(failures, errors.New("matching audio session has no recoverable identity"))
			return nil
		}
		if s.Muted {
			matched = true
			return nil
		}
		// Recovery needs the original state on disk before muting anything
		if !recorded[s.ID] {
			next := append(old, record{ID: s.ID, WasMuted: s.Muted})
			if err := write(path, next); err != nil {
				failures = errors.Join(failures, err)
				return nil
			}
			old = next
			recorded[s.ID] = true
		}
		// Keep successful mutes even if another session or endpoint fails
		if err := s.Set(true); err != nil {
			failures = errors.Join(failures, err)
		} else {
			matched = true
		}
		return nil
	})
	return matched, errors.Join(err, failures)
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
		// Restore only an exact identity match A similarly named session may be unrelated
		for _, entry := range records {
			if entry.ID == s.ID {
				// One failed restore should leave the other sessions eligible for recovery
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
	// Unplugging an audio device can hide the session while its process is still alive
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

// Treat an already absent journal as a successful cleanup
func clearJournal(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("clear mute journal: %w", err)
	}
	return nil
}
