// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/geo"
)

func TestDebugLocationShowsPreferredCrewAccent(t *testing.T) {
	s := &state{dir: t.TempDir(), debug: make(chan debugEvent, 1)}
	s.debugEnabled.Store(true)
	prefs := config.Defaults()
	prefs.Put("installed_packs", []string{packs[0].name})
	pos := &geo.Position{Country: "FR", Region: "Île-de-France"}
	assertLocation := func(pos *geo.Position, want string) {
		t.Helper()
		s.traceLocation(prefs, pos)
		if got := (<-s.debug).line; !strings.Contains(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	assertLocation(nil, "Aircraft location=unavailable | preferred crew accent=American")
	assertLocation(pos, "Aircraft location=Île-de-France, France | preferred crew accent=American")
	if err := os.WriteFile(filepath.Join(s.dir, "accent_mappings.json"), []byte(`[{"country":"France","accent_used":"British"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	assertLocation(pos, "Aircraft location=Île-de-France, France | preferred crew accent=British")
	prefs.Put("force_accent", "irish")
	assertLocation(pos, "Aircraft location=Île-de-France, France | preferred crew accent=Irish (forced)")
	// Force an accent outside the installed pack to exercise the fallback diagnostic.
	prefs.Put("force_accent", "Japanese")
	assertLocation(pos, "Aircraft location=Île-de-France, France | preferred crew accent=British (forced accent unavailable)")
}

func TestDebugTerminalSurvivesLogFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	// Block log creation by putting a file at the data directory path
	if err := os.WriteFile(dir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	s := &state{ctx: ctx, dir: dir, debug: make(chan debugEvent, 4)}
	s.debugEnabled.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.debugWriter(func() (io.WriteCloser, error) { return writer, nil })
	}()
	t.Cleanup(func() {
		cancel()
		// Unblock the pipe writer before waiting for its goroutine to finish.
		_ = reader.Close()
		<-done
	})
	lines := make(chan string, 8)
	go func() {
		scan := bufio.NewScanner(reader)
		for scan.Scan() {
			lines <- scan.Text()
		}
		close(lines)
	}()
	wantLine := func(want string) {
		t.Helper()
		select {
		case line := <-lines:
			if !strings.Contains(line, want) {
				t.Fatalf("got %q, want %q", line, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	s.trace("SYS", "First event")
	wantLine("Could not write debug.log")
	wantLine("First event")
	s.trace("SYS", "Second event")
	wantLine("Second event")
	// Remove the blocking file so the next event can reopen the log.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	s.trace("SYS", "Third event")
	wantLine("Writing to debug.log resumed")
	wantLine("Third event")
	data, err := os.ReadFile(filepath.Join(dir, "debug.log"))
	if err != nil || !strings.Contains(string(data), "Third event") {
		t.Fatalf("debug.log did not resume: %q, %v", data, err)
	}
}
