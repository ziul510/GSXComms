// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/ziul510/GSXCommsGo/internal/config"
	"golang.org/x/sys/windows"
)

// Queue a complete settings snapshot and a UI callback for the disk result
type saveRequest struct {
	values config.Values
	then   func(error)
}

type debugEvent struct {
	enabled bool
	line    string
}

// Apply one control immediately without saving unrelated edits still open in Settings.
func (s *state) saveSetting(key string, value any, then func(error)) {
	next := s.settingsDraft()
	next.Put(key, value)
	s.save(next, then)
}

func (s *state) settingsWriter() {
	defer close(s.saved)
	// Write snapshots in order so a slower write can't replace a newer one
	for req := range s.saves {
		err := config.Save(s.dir, req.values)
		// Apply settings on the window thread only after the writer returns
		s.async(func() { s.finishSave(req, err) })
	}
}

func (s *state) finishSave(req *saveRequest, err error) {
	// An older completion must leave the newest queued draft available
	if s.pendingSave == req {
		s.pendingSave = nil
	}
	if err != nil {
		s.log("ERR", "Could not save settings: "+err.Error())
	} else {
		// Side effects compare with the last saved settings, even if an earlier request failed
		previous := s.cfg
		s.cfg = req.values
		s.publishTransmissionPolicy()
		if !bytes.Equal(previous["ptt_bind"], s.cfg["ptt_bind"]) {
			// Stop recording before changing which key release the hook listens for
			s.record(false)
			captureKey.Store(bindingKey(config.Get[config.Binding](s.cfg, "ptt_bind")))
		}
		// Enabling or disabling debug output needs an event even when no log text changed
		if config.Get[bool](previous, "debug_mode") != config.Get[bool](s.cfg, "debug_mode") {
			s.setDebug(config.Get[bool](s.cfg, "debug_mode"))
		}
		if config.Get[bool](previous, "minimize_to_tray") != config.Get[bool](s.cfg, "minimize_to_tray") {
			s.configureTray()
		}
		if config.Get[bool](previous, "topmost_active") != config.Get[bool](s.cfg, "topmost_active") {
			s.setTopmost()
		}
		if config.Get[string](previous, "speech_provider") != config.Get[string](s.cfg, "speech_provider") {
			s.prepareSpeech(true, false)
		}
	}
	// Tell the originating dialog when it can close or show a write error
	if req.then != nil {
		req.then(err)
	}
}

func startDebugTerminal() (io.WriteCloser, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	// Windows chooses the host when it starts a new console process
	cmd := exec.Command(filepath.Join(filepath.Dir(exe), "gsxdebug.exe"))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = input.Close()
		return nil, err
	}
	go func() { _ = cmd.Wait() }()
	return input, nil
}

func (s *state) debugWriter() {
	// The writer owns the file and the terminal's input for the active debug session
	var file *os.File
	var output io.WriteCloser
	var terminalAttempted bool
	var size int64
	closeDebug := func() {
		if file != nil {
			_ = file.Close()
			file = nil
		}
		if output != nil {
			_ = output.Close()
			output = nil
		}
		terminalAttempted = false
	}
	defer closeDebug()
	// Append to any existing log and count its old bytes toward rotation
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
			// Switching debug off closes both outputs until an enabled event arrives
			if !event.enabled || !s.debugEnabled.Load() {
				closeDebug()
				continue
			}
			if file == nil {
				if err := openLog(); err != nil {
					continue
				}
			}
			if !terminalAttempted {
				terminalAttempted = true
				var err error
				output, err = startDebugTerminal()
				if err != nil {
					s.trace("SYS", "Could not open the debug terminal")
				}
			}
			// An enabled event with no text opens the debug output without writing a line
			if event.line == "" {
				continue
			}
			// Include the newline when deciding whether the next entry fits in the current log
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
				if _, err := fmt.Fprintln(output, event.line); err != nil {
					_ = output.Close()
					output = nil
				}
			}
		}
	}
}

func rotate(dir string) error {
	path := filepath.Join(dir, "debug.log")
	// Rotate oldest first so the next move can't overwrite an unshifted backup
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

func (s *state) setDebug(enabled bool) {
	s.debugEnabled.Store(enabled)
	s.debugLog(debugEvent{enabled: enabled})
	if enabled {
		prefs := s.cfg
		provider := config.Get[string](prefs, "speech_provider")
		switch provider {
		case "parakeet", "orukeet", "mistral":
		default:
			provider = "unknown"
		}
		s.trace("SYS", fmt.Sprintf("GSXComms starting diagnostics on %s/%s (%s)", runtime.GOOS, runtime.GOARCH, runtime.Version()))
		s.trace("SYS", fmt.Sprintf("Speech provider %s, accent packs %d, recording cap %ds",
			provider, len(config.Get[[]string](prefs, "installed_packs")), config.Get[int](prefs, "max_recording_seconds")))
		s.trace("SYS", fmt.Sprintf("Auto-connect=%t, operator selection=%t, Broader Mode=%t, crew mute=%t, PTT bound=%t",
			config.Get[bool](prefs, "auto_connect_sim"), config.Get[bool](prefs, "auto_select_operator"),
			config.Get[bool](prefs, "enable_menu_matching"), config.Get[bool](prefs, "mute_gsx_audio_during_crew_voice"),
			config.Get[config.Binding](prefs, "ptt_bind").Type != ""))
	}
}

// Diagnostics accept only app-generated facts so logs never persist transcript or credential text
func (s *state) trace(category, detail string) {
	if s.debugEnabled.Load() {
		s.debugLog(debugEvent{true, fmt.Sprintf("%s [%s] %s", time.Now().Format("2006-01-02 15:04:05.000"), category, detail)})
	}
}

func (s *state) traceFailure(category, operation string) {
	if !s.debugEnabled.Load() {
		return
	}
	s.trace(category, operation)
	// Go function names and line numbers provide context without arguments or paths
	var locations [6]uintptr
	count := runtime.Callers(2, locations[:])
	frames := runtime.CallersFrames(locations[:count])
	for {
		frame, more := frames.Next()
		if frame.Function != "" {
			s.trace(category, fmt.Sprintf("  %s:%d", frame.Function, frame.Line))
		}
		if !more {
			break
		}
	}
}

func (s *state) debugLog(event debugEvent) {
	select {
	// The UI must keep running even when the debug writer can't keep up
	case s.debug <- event:
	default:
	}
}
