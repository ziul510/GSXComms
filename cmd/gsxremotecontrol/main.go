// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/ziul510/GSXCommsGo/internal/gsx"
	"golang.org/x/sys/windows"
)

func main() {
	// Both front ends close the shared GSX session when interrupted
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	session := gsx.New(nil, gsx.Panel())
	defer session.Close()
	if len(os.Args) > 1 && os.Args[1] == "--ipc" {
		ipc(ctx, session, os.Stdin, os.Stdout, connect)
		return
	}
	console(ctx, session)
}

// Leave the current transport in place until the health loop closes it.
func connect(ctx context.Context, s *gsx.Session) {
	if !s.Connected() {
		s.Sim, _ = gsx.Connect(ctx)
	}
}

// Both console modes must drop dead transports before connect can try again.
func pollConnection(ctx context.Context, s *gsx.Session, dial func(context.Context, *gsx.Session), failures *int) {
	if !s.Connected() {
		dial(ctx, s)
		*failures = 0
		return
	}
	health, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, _, err := s.Status(health)
	// A missing speed value can occur while SimConnect is still healthy.
	if err == nil || errors.Is(err, gsx.ErrSpeedUnavailable) {
		*failures = 0
		return
	}
	*failures = *failures + 1
	if errors.Is(err, gsx.ErrDisconnected) || *failures >= 3 {
		_ = s.Close()
		*failures = 0
	}
}

func ipc(ctx context.Context, s *gsx.Session, in io.Reader, out io.Writer, connect func(context.Context, *gsx.Session)) {
	connect(ctx, s)
	send := json.NewEncoder(out)
	msg := func(p map[string]any) { _ = send.Encode(p) }
	// Send ready so clients can ping even while SimConnect is offline
	msg(map[string]any{"status": "ok", "ready": true, "connected": s.Connected()})
	// Read stdin separately so idle clients can't block simulator polling
	lines := make(chan string)
	go func() {
		defer close(lines)
		r := bufio.NewScanner(in)
		for r.Scan() {
			select {
			case lines <- strings.TrimSpace(r.Text()):
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := time.Now()
	healthFailures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Release GSX if the IPC client stops sending requests
			if s.RemoteActive() && time.Since(last) >= 10*time.Second {
				_ = s.Release(ctx)
			}
			pollConnection(ctx, s, connect, &healthFailures)
		case line, ok := <-lines:
			if !ok {
				return
			}
			// Count input as activity even if a blank line skips the command switch
			last = time.Now()
			switch {
			case line == "":
				continue
			case line == "PING":
				msg(map[string]any{"status": "ok", "connected": s.Connected()})
			// Report success only after choices arrive in the panel file
			case line == "OPEN_MENU":
				_, err := s.OpenMenuPage(ctx)
				msg(map[string]any{"status": "ok", "menu_opened": err == nil})
			case line == "CLOSE_MENU":
				msg(map[string]any{"status": "ok", "menu_closed": s.Release(ctx) == nil})
			case line == "GET_MENU":
				menu := s.ReadMenu()
				msg(map[string]any{"status": "ok", "title": menu.Title, "options": menu.Options, "active": menu.Active, "remote_active": s.RemoteActive()})
			// IPC clients supply the zero-based index expected by the transport
			case strings.HasPrefix(line, "SELECT:"):
				n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "SELECT:")))
				if err != nil {
					msg(map[string]any{"status": "error", "message": "Invalid SELECT command: " + line})
					continue
				}
				msg(map[string]any{"status": "ok", "selected": n, "success": s.Choose(ctx, n) == nil})
			case line == "SHUTDOWN":
				msg(map[string]any{"status": "ok", "shutdown": true})
				return
			default:
				msg(map[string]any{"status": "error", "message": "Unknown command: " + line})
			}
			// Start the idle timer again after commands that can take several seconds
			last = time.Now()
		}
	}
}

func console(ctx context.Context, s *gsx.Session) {
	// Read single keystrokes so the menu stays interactive without Enter
	crt := windows.NewLazySystemDLL("msvcrt.dll")
	hit, read := crt.NewProc("_kbhit"), crt.NewProc("_getch")
	if hit.Find() != nil || read.Find() != nil {
		return
	}
	fmt.Print("\x1b[2J\x1b[H")
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	var frame string
	var last, nextPoll time.Time
	healthFailures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if time.Now().After(nextPoll) {
			pollConnection(ctx, s, connect, &healthFailures)
			nextPoll = time.Now().Add(time.Second)
		}
		for key, _, _ := hit.Call(); key != 0; key, _, _ = hit.Call() {
			k, _, _ := read.Call()
			last = time.Now()
			switch {
			case k == 27 || k == 'q' || k == 'Q':
				return
			// The CRT sends a prefix byte for extended keys Consume its second byte too
			case k == 0 || k == 224:
				_, _, _ = read.Call()
			case k == 'm' || k == 'M':
				_ = s.Toggle(ctx)
			case k == '0':
				_ = s.Release(ctx)
			// Console keys are one-based GSX takes zero-based choices
			case k >= '1' && k <= '9':
				_ = s.Choose(ctx, int(k-'1'))
			}
		}
		next := ""
		// Use ownership to clear stale rows after an idle release or disconnect.
		if s.RemoteActive() && time.Since(last) >= 5*time.Second {
			_ = s.Release(ctx)
		}
		if s.RemoteActive() {
			var b strings.Builder
			for i, op := range s.ReadMenu().Options {
				fmt.Fprintf(&b, "%d. %s\n", i+1, op)
			}
			next = b.String()
		}
		// Avoid clearing the console and repainting an identical menu every tick
		if next != frame {
			fmt.Print("\x1b[2J\x1b[H", next)
			frame = next
		}
	}
}
