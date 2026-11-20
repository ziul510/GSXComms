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

func connect(ctx context.Context, s *gsx.Session) {
	if !s.Connected() {
		s.Sim, _ = gsx.Connect(ctx)
	}
}

func ipc(ctx context.Context, s *gsx.Session, in io.Reader, out io.Writer, connect func(context.Context, *gsx.Session)) {
	connect(ctx, s)
	send := json.NewEncoder(out)
	msg := func(p map[string]any) { _ = send.Encode(p) }
	// Send ready so clients can ping even while SimConnect is offline.
	msg(map[string]any{"status": "ok", "ready": true, "connected": s.Connected()})
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
			if s.RemoteActive() && time.Since(last) >= 10*time.Second {
				_ = s.Release(ctx)
			}
			connect(ctx, s)
			if !s.Connected() {
				continue
			}
			if _, _, err := s.Status(ctx); err != nil && !errors.Is(err, gsx.ErrSpeedUnavailable) {
				healthFailures++
				if healthFailures >= 3 {
					_ = s.Close()
					healthFailures = 0
				}
			} else {
				healthFailures = 0
			}
		case line, ok := <-lines:
			if !ok {
				return
			}
			last = time.Now()
			switch {
			case line == "":
				continue
			case line == "PING":
				msg(map[string]any{"status": "ok", "connected": s.Connected()})
			case line == "OPEN_MENU":
				err := s.Open(ctx)
				if err == nil {
					_, err = s.WaitMenu(ctx)
				}
				msg(map[string]any{"status": "ok", "menu_opened": err == nil})
			case line == "CLOSE_MENU":
				msg(map[string]any{"status": "ok", "menu_closed": s.Release(ctx) == nil})
			case line == "GET_MENU":
				menu := s.ReadMenu()
				msg(map[string]any{"status": "ok", "title": menu.Title, "options": menu.Options, "active": menu.Active, "remote_active": s.RemoteActive()})
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
			last = time.Now()
		}
	}
}

func console(ctx context.Context, s *gsx.Session) {
	crt := windows.NewLazySystemDLL("msvcrt.dll")
	hit, read := crt.NewProc("_kbhit"), crt.NewProc("_getch")
	if hit.Find() != nil || read.Find() != nil {
		return
	}
	fmt.Print("\x1b[2J\x1b[H")
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	var frame string
	var last, nextConnect time.Time
	active := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if time.Now().After(nextConnect) && !s.Connected() {
			connect(ctx, s)
			nextConnect = time.Now().Add(time.Second)
		}
		for key, _, _ := hit.Call(); key != 0; key, _, _ = hit.Call() {
			k, _, _ := read.Call()
			last = time.Now()
			switch {
			case k == 27 || k == 'q' || k == 'Q':
				return
			case k == 0 || k == 224:
				_, _, _ = read.Call()
			case k == 'm' || k == 'M':
				active = true
				_ = s.Toggle(ctx)
			case k == '0':
				_ = s.Release(ctx)
			case k >= '1' && k <= '9':
				_ = s.Choose(ctx, int(k-'1'))
			}
		}
		next := ""
		if active && time.Since(last) >= 5*time.Second {
			active = false
			_ = s.Release(ctx)
		}
		if active {
			var b strings.Builder
			for i, op := range s.ReadMenu().Options {
				fmt.Fprintf(&b, "%d. %s\n", i+1, op)
			}
			next = b.String()
		}
		if next != frame {
			fmt.Print("\x1b[2J\x1b[H", next)
			frame = next
		}
	}
}
