// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/geo"
	"github.com/ziul510/GSXCommsGo/internal/gsx"
	"github.com/ziul510/GSXCommsGo/internal/speech"
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

// Apply one control immediately without saving unrelated edits still open in Settings
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
		s.traceFailure("SYS", "Could not save settings", err)
		s.log("ERR", "Could not save settings: "+err.Error())
	} else {
		// Side effects compare with the last saved settings, even if an earlier request failed
		previous := s.cfg
		s.cfg = req.values
		s.publishTransmissionPolicy()
		if config.Get[bool](previous, config.MenuReadoutSetting) != config.Get[bool](s.cfg, config.MenuReadoutSetting) {
			// Invalidate requests from the previous setting, even after re-enabling.
			s.readoutGeneration.Add(1)
			if s.readouts.cancel != nil {
				s.readouts.cancel()
			}
			s.readouts = newReadoutScope(s.ctx)
			s.audioMu.Lock()
			for i := len(s.audioQueue) - 1; i >= 0; i-- {
				if s.audioQueue[i].dry {
					s.dropAudio(i)
				}
			}
			s.audioMu.Unlock()
			if s.presetReadoutChanged != nil {
				s.presetReadoutChanged()
			}
		}
		if !bytes.Equal(previous["ptt_bind"], s.cfg["ptt_bind"]) {
			// Replacing a binding discards any capture started with it.
			s.stopRecording(errPTTBindingChanged)
			captureKey.Store(bindingKey(config.Get[config.Binding](s.cfg, "ptt_bind")))
		}
		// Enabling or disabling debug output needs an event even when no log text changed
		if config.Get[bool](previous, "debug_mode") != config.Get[bool](s.cfg, "debug_mode") {
			s.setDebug(config.Get[bool](s.cfg, "debug_mode"))
		} else if s.debugEnabled.Load() && (!bytes.Equal(previous["force_accent"], s.cfg["force_accent"]) ||
			!bytes.Equal(previous["fallback_accent"], s.cfg["fallback_accent"]) || !bytes.Equal(previous["installed_packs"], s.cfg["installed_packs"])) {
			go s.traceLocation(s.cfg, s.location.Load())
		}
		if config.Get[bool](previous, "minimize_to_tray") != config.Get[bool](s.cfg, "minimize_to_tray") {
			s.configureTray()
		}
		if config.Get[bool](previous, "topmost_active") != config.Get[bool](s.cfg, "topmost_active") {
			s.setTopmost()
		}
		// Keep the loaded recognizer when an unrelated setting changes.
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

func (s *state) debugWriter(startTerminal func() (io.WriteCloser, error)) {
	// The writer owns the file and the terminal's input for the active debug session
	var file *os.File
	var output io.WriteCloser
	var terminalAttempted, logFailed bool
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
		terminalAttempted, logFailed = false, false
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
	writeLog := func(line string) error {
		if file == nil {
			if err := openLog(); err != nil {
				return err
			}
		}
		// Include the newline when deciding whether the entry fits
		if size+int64(len(line)+1) > 2_000_000 {
			_ = file.Close()
			file = nil
			if err := rotate(s.dir); err != nil {
				return err
			}
			if err := openLog(); err != nil {
				return err
			}
		}
		n, err := fmt.Fprintln(file, line)
		size += int64(n)
		return err
	}
	writeTerminal := func(line string) {
		if output != nil {
			if _, err := fmt.Fprintln(output, line); err != nil {
				_ = output.Close()
				output = nil
			}
		}
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
			// Try to open the terminal once per debug session while the file logger keeps working.
			if !terminalAttempted {
				terminalAttempted = true
				var err error
				output, err = startTerminal()
				if err != nil {
					_ = writeLog(debugLine("WARN", "SYS", "Could not open the debug terminal: "+debugReason(err)))
				}
			}
			if err := writeLog(event.line); err != nil {
				if file != nil {
					_ = file.Close()
					file = nil
				}
				if !logFailed {
					writeTerminal(debugLine("WARN", "SYS", "Could not write debug.log: "+debugReason(err)))
				}
				logFailed = true
			} else if logFailed {
				writeTerminal(debugLine("INFO", "SYS", "Writing to debug.log resumed"))
				logFailed = false
			}
			writeTerminal(event.line)
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
	if !enabled {
		// Wake the debug writer on disable so it can close files even with no new messages.
		s.debugLog(debugEvent{})
		return
	}
	build := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, entry := range info.Settings {
			if entry.Key == "vcs.revision" {
				build = " commit=" + entry.Value[:min(12, len(entry.Value))]
				break
			}
		}
	}
	prefs := s.cfg
	s.trace("SYS", fmt.Sprintf("GSXComms debug session | %s/%s %s%s | log=debug.log", runtime.GOOS, runtime.GOARCH, runtime.Version(), build))
	s.trace("SIM", fmt.Sprintf("Connected=%t, GSX ready=%t, auto-connect=%t", s.connected, s.ready, config.Get[bool](prefs, "auto_connect_sim")))
	s.trace("GSX", fmt.Sprintf("Auto-select operator=%t, Broader Mode=%t, menu readout=%t",
		config.Get[bool](prefs, "auto_select_operator"), config.Get[bool](prefs, "enable_menu_matching"),
		config.Get[bool](prefs, config.MenuReadoutSetting)))
	s.trace("STT", fmt.Sprintf("Provider=%s, ready=%t, loading=%t, recording cap=%ds", debugProvider(config.Get[string](prefs, "speech_provider")),
		s.speechReady, s.speechLoading != "", config.Get[int](prefs, "max_recording_seconds")))
	s.trace("PTT", fmt.Sprintf("Binding=%s, simulation=%t, ignore speed limit=%t, fresh speed=%t", debugBinding(config.Get[config.Binding](prefs, "ptt_bind").Type),
		config.Get[bool](prefs, "simulation_mode"), config.Get[bool](prefs, "ignore_speed_restriction"), s.groundSpeed.Load().fresh()))
	s.trace("AUDIO", fmt.Sprintf("Accent packs=%d, crew mute=%t, default mic=%t, default speakers=%t",
		len(config.Get[[]string](prefs, "installed_packs")), config.Get[bool](prefs, "mute_gsx_audio_during_crew_voice"),
		defaultAudioSelection(config.Get[string](prefs, "input_device_name"), config.Get[string](prefs, "input_device_id")),
		defaultAudioSelection(config.Get[string](prefs, "output_device_name"), config.Get[string](prefs, "output_device_id"))))
	go s.traceLocation(prefs, s.location.Load())
}

func (s *state) traceLocation(prefs config.Values, pos *geo.Position) {
	if !s.debugEnabled.Load() {
		return
	}
	label := ""
	if enabled := config.Get[[]string](prefs, "installed_packs"); len(enabled) > 0 {
		for _, candidate := range crewAccentCandidates(prefs, pos, geo.Overrides(s.dir)) {
			if _, name := installedCrewAccent(enabled, candidate); name != "" {
				label = name
				break
			}
		}
	}
	// Playback reports the actual accent after a matching recording is found
	s.trace("GEO", fmt.Sprintf("Aircraft location=%s | preferred crew accent=%s", pos.Label(), debugCrewAccent(prefs, label)))
}

// Mark a force only when the preferred or played accent matches the override
func debugCrewAccent(prefs config.Values, name string) string {
	forced := config.Get[string](prefs, "force_accent")
	if name != "" && strings.EqualFold(forced, name) {
		return name + " (forced)"
	}
	if name == "" {
		name = "unavailable"
	}
	if forced != "" && forced != "Disabled" {
		return name + " (forced accent unavailable)"
	}
	return name
}

func debugBinding(binding string) string {
	switch binding {
	case "key":
		return "keyboard"
	case "joystick":
		return "controller"
	case "":
		return "none"
	default:
		// Use a fixed label for unrecognized binding types so raw settings stay out of diagnostics.
		return "unknown"
	}
}

func debugProvider(provider string) string {
	switch provider {
	// Allow only known provider names in logs because the saved value could contain arbitrary text.
	case "parakeet", "orukeet", "mistral":
		return provider
	default:
		return "unknown"
	}
}

func debugLine(level, category, detail string) string {
	return fmt.Sprintf("%s  %-4s  %-5s  %s", time.Now().Format("2006-01-02 15:04:05.000"), level, category, detail)
}

// Trace only internal facts and keep conversation content out of diagnostics
func (s *state) trace(category, detail string) {
	s.traceLevel("INFO", category, detail)
}

func (s *state) traceLevel(level, category, detail string) {
	if s.debugEnabled.Load() {
		s.debugLog(debugEvent{true, debugLine(level, category, detail)})
	}
}

func (s *state) traceFailure(category, operation string, err error) {
	if s.debugEnabled.Load() {
		s.traceLevel("WARN", category, operation+": "+debugReason(err))
	}
}

// Use typed causes because error strings can contain paths or worker and API replies
func debugReason(err error) string {
	var code syscall.Errno
	var audio waveError
	var mistral *speech.MistralError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, os.ErrNotExist):
		return "required file or device unavailable"
	case errors.Is(err, os.ErrPermission):
		return "access denied"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected end of response"
	case errors.Is(err, gsx.ErrDisconnected):
		return "SimConnect disconnected"
	case errors.Is(err, gsx.ErrMenuChanged):
		return "menu changed before selection"
	case errors.Is(err, gsx.ErrHotfix):
		return "GSX hotfix required"
	case errors.Is(err, gsx.ErrMenuInactive):
		return "GSX did not populate the menu"
	case errors.Is(err, gsx.ErrAdditionalMenuInactive):
		return "GSX did not open the Additional Services menu"
	case errors.Is(err, gsx.ErrNoMenuMatch):
		return "no matching menu option"
	case errors.Is(err, errMicUnavailable):
		return "selected microphone unavailable"
	case errors.As(err, &code):
		return fmt.Sprintf("Windows error %d", code)
	case errors.As(err, &audio):
		return audio.Error()
	case errors.As(err, &mistral):
		return mistral.Error()
	default:
		// Keep raw errors out of the debug log because they can contain paths or user input.
		return "see the in-app message for details"
	}
}

func (s *state) debugLog(event debugEvent) {
	select {
	// The UI must keep running even when the debug writer can't keep up
	case s.debug <- event:
	default:
	}
}
