// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ziul510/GSXCommsGo/internal/config"
	"golang.org/x/sys/windows"
)

type saveRequest struct {
	values config.Values
	then   func(error)
}

type debugEvent struct {
	enabled bool
	line    string
}

func (s *state) settingsWriter() {
	defer close(s.saved)
	// Write snapshots in order so a slower write can't replace a newer one.
	for req := range s.saves {
		err := config.Save(s.dir, req.values)
		s.async(func() { s.finishSave(req, err) })
	}
}

func (s *state) finishSave(req *saveRequest, err error) {
	// An older completion must leave the newest queued draft available.
	if s.pendingSave == req {
		s.pendingSave = nil
	}
	if err != nil {
		s.log("ERR", "Could not save settings: "+err.Error())
	} else {
		previous := s.cfg
		s.cfg = req.values
		if !bytes.Equal(previous["ptt_bind"], s.cfg["ptt_bind"]) {
			// Stop recording before changing which key release the hook listens for.
			s.record(false)
			captureKey.Store(bindingKey(config.Get[config.Binding](s.cfg, "ptt_bind")))
		}
		if config.Get[bool](previous, "debug_mode") != config.Get[bool](s.cfg, "debug_mode") {
			s.debugLog(debugEvent{enabled: config.Get[bool](s.cfg, "debug_mode")})
		}
		if config.Get[bool](previous, "minimize_to_tray") != config.Get[bool](s.cfg, "minimize_to_tray") {
			s.configureTray()
		}
		if config.Get[bool](previous, "topmost_active") != config.Get[bool](s.cfg, "topmost_active") {
			s.setTopmost()
		}
	}
	if req.then != nil {
		req.then(err)
	}
}

func (s *state) debugWriter() {
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	newConsole, freeConsole := kernel.NewProc("AllocConsole"), kernel.NewProc("FreeConsole")
	var file, output *os.File
	var size int64
	closeDebug := func() {
		if file != nil {
			_ = file.Close()
			file = nil
		}
		if output != nil {
			_ = output.Close()
			output = nil
			_, _, _ = freeConsole.Call()
		}
	}
	defer closeDebug()
	openLog := func() error {
		if err := os.MkdirAll(s.dir, 0700); err != nil {
			return err
		}
		handle, err := os.OpenFile(filepath.Join(s.dir, "debug.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		info, err := handle.Stat()
		if err != nil {
			_ = handle.Close()
			return err
		}
		file, size = handle, info.Size()
		return nil
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case event := <-s.debug:
			if !event.enabled {
				closeDebug()
				continue
			}
			if file == nil {
				if err := openLog(); err != nil {
					continue
				}
				if ok, _, _ := newConsole.Call(); ok != 0 {
					output, _ = os.OpenFile("CONOUT$", os.O_WRONLY, 0)
				}
			}
			// An enabled event with no text opens the debug output without writing a line.
			if event.line == "" {
				continue
			}
			if size+int64(len(event.line)+1) > 2_000_000 {
				_ = file.Close()
				file = nil
				if err := rotate(s.dir); err != nil {
					continue
				}
				if err := openLog(); err != nil {
					continue
				}
			}
			n, _ := fmt.Fprintln(file, event.line)
			size += int64(n)
			if output != nil {
				_, _ = fmt.Fprintln(output, event.line)
			}
		}
	}
}

func rotate(dir string) error {
	path := filepath.Join(dir, "debug.log")
	// Rotate oldest first so the next move can't overwrite an unshifted backup.
	for n := 2; n >= 0; n-- {
		source := path
		if n > 0 {
			source = fmt.Sprintf("%s.%d", path, n)
		}
		err := config.Replace(source, fmt.Sprintf("%s.%d", path, n+1))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *state) debugLog(event debugEvent) {
	select {
	// The UI must keep running even when the debug writer can't keep up.
	case s.debug <- event:
	default:
	}
}
