// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package mute

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type mock struct {
	id     identity
	muted  bool
	before func(bool)
}

func (m *mock) Alive(id identity) bool { return m.id == id }
func (m *mock) Visit(visit func(audioSession) error) error {
	return visit(audioSession{m.id, m.muted, "Couatl64_MSFS", func(next bool) error {
		if m.before != nil {
			m.before(next)
		}
		m.muted = next
		return nil
	}})
}
func TestJournalSurvivesPlayer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	m := &mock{id: identity{"endpoint", "instance", 35, 9}}
	// Inspect the journal at the moment the backend is about to change mute state.
	m.before = func(next bool) {
		if next {
			journal, err := read(path)
			if err != nil || len(journal) != 1 || journal[0].WasMuted || journal[0].ID != m.id {
				t.Fatal("muted before durable recovery record was available")
			}
		}
	}
	if err := acquire(path, m); err != nil || !m.muted {
		t.Fatalf("acquire: %v, muted=%v", err, m.muted)
	}
	if err := restore(path, m); err != nil || m.muted {
		t.Fatalf("recovery: %v, muted=%v", err, m.muted)
	}
	if records, err := read(path); err != nil || len(records) != 0 {
		t.Fatal("recovered entry still pending")
	}
}
func TestFailedWriteLeavesAudioAlone(t *testing.T) {
	// A file in place of the parent directory forces the journal write to fail.
	parent := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(parent, nil, 0600); err != nil {
		t.Fatal(err)
	}
	m := &mock{id: identity{"endpoint", "instance", 35, 9}}
	if err := acquire(filepath.Join(parent, "journal.json"), m); err == nil || m.muted {
		t.Fatal("must report failed write and leave session untouched")
	}
}

type sessions []audioSession

func (s sessions) Alive(id identity) bool {
	for _, session := range s {
		if session.ID == id {
			return true
		}
	}
	return false
}

func (s sessions) Visit(visit func(audioSession) error) error {
	for _, session := range s {
		if err := visit(session); err != nil {
			return err
		}
	}
	return nil
}

func TestRestoreContinuesPastUnavailableSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	first := identity{"endpoint", "first", 35, 9}
	second := identity{"endpoint", "second", 35, 9}
	api := sessions{
		{ID: first, Set: func(bool) error { return errors.New("temporarily unavailable") }},
		{ID: second, Set: func(bool) error { return nil }},
	}
	if err := write(path, []record{{ID: first}, {ID: second}}); err != nil {
		t.Fatal(err)
	}
	if err := restore(path, api); err == nil {
		t.Fatal("expected restoration error")
	}
	remaining, err := read(path)
	if err != nil || len(remaining) != 1 || remaining[0].ID != first {
		t.Fatalf("pending=%v err=%v", remaining, err)
	}
	api[0].Set = func(bool) error { return nil }
	if err := restore(path, api); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed journal still present: %v", err)
	}
}

type hiddenSession struct {
	*mock
	hidden identity
}

func (h hiddenSession) Alive(id identity) bool { return id == h.hidden || h.mock.Alive(id) }

func TestPendingEndpointDoesNotBlockNewSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	hidden := identity{"unplugged", "old", 35, 9}
	active := &mock{id: identity{"headphones", "new", 35, 9}}
	// Keep the old process alive but leave its unplugged endpoint out of enumeration.
	api := hiddenSession{active, hidden}
	if err := write(path, []record{{ID: hidden}}); err != nil {
		t.Fatal(err)
	}
	if err := acquire(path, api); err != nil || !active.muted {
		t.Fatalf("acquire muted=%t err=%v", active.muted, err)
	}
	pending, err := read(path)
	if err != nil || len(pending) != 2 || pending[0].ID != hidden || pending[1].ID != active.id {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	if err := restore(path, api); err != nil || active.muted {
		t.Fatalf("restore muted=%t err=%v", active.muted, err)
	}
	pending, err = read(path)
	if err != nil || len(pending) != 1 || pending[0].ID != hidden {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
}
