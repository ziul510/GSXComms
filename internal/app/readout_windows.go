// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/gsx"
	"github.com/ziul510/GSXCommsGo/internal/models"
	"golang.org/x/sys/windows"
)

var menuPunct = regexp.MustCompile(`[^a-zA-Z0-9\s]`)

func formatReadout(options []string) string {
	numbers := []string{"One", "Two", "Three", "Four", "Five", "Six", "Seven", "Eight", "Nine"}
	items := make([]string, 0, len(options))
	for i, text := range options {
		// Strip punctuation that would be awkward in spoken menu labels
		words := strings.Join(strings.Fields(menuPunct.ReplaceAllString(text, " ")), " ")
		if words == "" {
			continue
		}
		// Keep the actual menu number even if an earlier label had nothing to pronounce
		prefix := fmt.Sprint(i + 1)
		if i < len(numbers) {
			prefix = numbers[i]
		}
		items = append(items, prefix+": "+words)
	}
	if len(items) == 0 {
		return "The GSX menu has no entries."
	}
	// Separate items into sentences so Kokoro can pause between choices
	return strings.Join(items, ". ") + "."
}

func readoutMatches(prefs config.Values, values ...string) bool {
	if !config.Get[bool](prefs, config.MenuReadoutSetting) {
		return false
	}
	// An empty setting leaves the special readout path disabled
	keyword := strings.TrimSpace(strings.ToLower(config.Get[string](prefs, "gsx_menu_reader_keyword")))
	if keyword == "" {
		return false
	}
	// Quote the user phrase and match it at word boundaries
	pattern := regexp.MustCompile(`(?:^|[^\pL\pN_])` + regexp.QuoteMeta(keyword) + `(?:$|[^\pL\pN_])`)
	for _, v := range values {
		if pattern.MatchString(strings.ToLower(v)) {
			return true
		}
	}
	return false
}

func (s *state) sayMenu(ctx context.Context, session *gsx.Session, prefs config.Values, onPresetQueued func()) bool {
	log := func(sender, note string) { s.async(func() { s.log(sender, note) }) }
	inPreset := onPresetQueued != nil
	if !session.Connected() {
		s.traceLevel("WARN", "TTS", "Menu readout blocked: SimConnect disconnected")
		log("SYS", "SimConnect handshake required to read the GSX menu")
		return false
	}
	if !s.menuReaderReady() {
		return false
	}
	// Couatl can still be replacing the panel after a selection
	if since := time.Since(s.menuActivity); since < 4*time.Second {
		if err := session.Sleep(ctx, 4*time.Second-since); err != nil {
			return false
		}
	}
	if !inPreset {
		select {
		// Reserve the reader through synthesis so repeated standalone requests don't pile up
		case s.speaking <- struct{}{}:
		default:
			s.trace("TTS", "Menu readout already in progress")
			log("SYS", "GSX menu readout is already in progress")
			return false
		}
	}
	// Reuse the menu if it is already populated Open a temporary one only when needed
	options := session.ReadMenu().Options
	if len(options) == 0 {
		menu, err := session.OpenMenuPage(ctx)
		// Release the menu opened for readout before starting the slower speech work
		_ = session.Release(ctx)
		if err != nil {
			if !inPreset {
				<-s.speaking
			}
			s.traceFailure("TTS", "Could not open the menu for readout", err)
			log("ERR", err.Error())
			return false
		}
		options = menu.Options
	}
	text := formatReadout(options)
	if inPreset {
		// Queue through the same window thread as the neighboring service acknowledgments
		s.async(func() {
			if config.Get[bool](s.cfg, config.MenuReadoutSetting) &&
				s.queueAudio(playback{prefs: prefs, readout: text, readoutEntries: len(options), dry: true, ordered: true}, false) {
				onPresetQueued()
			}
		})
		return true
	}
	// Synthesizing speech is slower than GSX polling, so give the simulator queue back now
	go func() {
		defer func() { <-s.speaking }()
		if pcm, rate := s.synthesizeReadout(s.ctx, text, len(options)); len(pcm) > 0 {
			// Queue readout audio behind waiting priority crew cues
			s.queueAudio(playback{prefs: prefs, pcm: pcm, rate: rate, dry: true}, false)
		}
	}()
	return true
}

func (s *state) menuReaderReady() bool {
	m, _ := models.Lookup("kokoro")
	// Require a verified Kokoro bundle before queueing a menu readout.
	if m.Ready(s.dir) {
		return true
	}
	s.traceLevel("WARN", "TTS", "Menu readout blocked: Kokoro model unavailable")
	s.async(func() {
		s.log("ERR", "Kokoro menu-reader model is unavailable. Open Settings to download or repair it")
	})
	return false
}

func (s *state) synthesizeReadout(ctx context.Context, text string, entries int) ([]byte, int) {
	log := func(sender, note string) { s.async(func() { s.log(sender, note) }) }
	m, _ := models.Lookup("kokoro")
	start := time.Now()
	// Use the pinned revision as the worker signature so repeated readouts can reuse the model.
	if err := s.reader.Load(ctx, m.Revision, m.Paths(s.dir), nil); err != nil {
		s.traceFailure("TTS", "Could not load the Kokoro worker", err)
		log("ERR", "GSX menu readout failed: "+err.Error())
		return nil, 0
	}
	pcm, rate, err := s.reader.Synthesize(ctx, text)
	if err != nil {
		s.traceFailure("TTS", "Could not synthesize the menu readout", err)
		log("ERR", "GSX menu readout failed: "+err.Error())
		return nil, 0
	}
	engine := s.reader.Runtime()
	log("GSX", fmt.Sprintf("Reading %d GSX menu entries aloud (%s)", entries, engine))
	if engine != "CPU" && engine != "CUDA" {
		engine = "unknown"
	}
	s.trace("TTS", fmt.Sprintf("Synthesized %d menu entries in %s on %s", entries, time.Since(start).Round(time.Millisecond), engine))
	return pcm, rate
}

func (s *state) output(ctx context.Context, next playback) error {
	if next.readout != "" {
		if !s.menuReaderReady() {
			return nil
		}
		next.pcm, next.rate = s.synthesizeReadout(ctx, next.readout, next.readoutEntries)
		if len(next.pcm) == 0 || ctx.Err() != nil {
			return nil
		}
	}
	var ready <-chan struct{}
	if !next.dry && next.cue != "error" {
		var release func()
		ready, release = s.muteCrew(ctx, next.prefs, 3950*time.Millisecond)
		defer release()
	}
	gain := min(1, config.Get[float64](next.prefs, "volume_voice")*normalizationGain(next.pcm))
	static := 0.0
	if !next.dry {
		if config.Get[bool](next.prefs, "enable_static") {
			static = config.Get[float64](next.prefs, "volume_static")
		}
	}
	channels := 1
	if next.cue != "" {
		channels = 2
	}
	// Mix into a copy so replaying a cached clip starts with the original samples.
	pcm := bytes.Clone(next.pcm)
	mixRadio(pcm, gain, static, next.rate, channels)
	waitForMute(ctx, ready)
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.debugEnabled.Load() {
		seconds := float64(len(pcm)) / float64(2*next.rate*channels)
		detail := fmt.Sprintf("Playing %.2fs of audio at %d Hz, %d channel(s)", seconds, next.rate, channels)
		if next.crewAccent != "" {
			detail += " | crew accent=" + debugCrewAccent(next.prefs, next.crewAccent)
		}
		s.trace("AUDIO", detail)
	}
	return playPCMBuffer(ctx, config.Get[string](next.prefs, "output_device_name"), config.Get[string](next.prefs, "output_device_id"), pcm, next.rate, channels, false)
}

// Give prompt acquisitions time to mute without stalling crew playback
func waitForMute(ctx context.Context, ready <-chan struct{}) {
	if ready == nil {
		return
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ready:
	case <-ctx.Done():
	case <-timer.C:
	}
}

// Match the legacy RMS gain limits without turning silence into noise
func normalizationGain(pcm []byte) float64 {
	var sum float64
	for i := 0; i+1 < len(pcm); i += 2 {
		sample := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		sum += sample * sample
	}
	if sum == 0 {
		return 1
	}
	// Target a common RMS level with limits to keep very quiet clips from getting too loud.
	return max(0.5, min(2.5, 3000/math.Sqrt(sum/float64(len(pcm)/2))))
}

const staticRate = 44100

var radioStatic = sync.OnceValue(func() []int16 {
	samples := make([]int16, 4*staticRate)
	for i := range samples {
		samples[i] = int16(rand.IntN(2401) - 1200)
	}
	return samples
})

func mixRadio(pcm []byte, gain, static float64, rate, channels int) {
	var noise []int16
	if static > 0 {
		noise = radioStatic()
	}
	for frame := 0; frame < len(pcm)/(2*channels); frame++ {
		offset := frame * 2 * channels
		add := 0.0
		if len(noise) > 0 {
			// Read noise at the output rate and repeat the same buffer when a clip runs longer.
			index := frame * staticRate / rate % len(noise)
			add = float64(noise[index]) * static
		}
		for ch := range channels {
			sample := pcm[offset+2*ch:]
			value := float64(int16(binary.LittleEndian.Uint16(sample)))*gain + add
			binary.LittleEndian.PutUint16(sample, uint16(int16(max(-32768, min(32767, value)))))
		}
	}
}

// Run a separate process so GSX audio recovery can survive the player exiting
func watchdog(dir string, restore bool) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	// Both playback and recovery target the same settings directory and mute journal
	args := []string{"--data-dir", dir}
	if restore {
		args = append(args, "--restore")
	}
	cmd := exec.Command(filepath.Join(filepath.Dir(exe), "gsxmute.exe"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd, nil
}

// Run the mute window and report failures through the app log
func (s *state) runMute(ctx context.Context, ready func()) {
	const message = "Could not mute Couatl/GSX audio"
	start := time.Now()
	timer := time.AfterFunc(2500*time.Millisecond, func() {
		if ctx.Err() == nil {
			s.traceLevel("WARN", "AUDIO", "Mute watchdog has not replied after 2.5s")
		}
	})
	defer timer.Stop()
	err := s.runWatchdog(ctx, false, func() {
		timer.Stop()
		ready()
		s.trace("AUDIO", fmt.Sprintf("Mute watchdog finished its first scan after %s", time.Since(start).Round(time.Millisecond)))
	})
	if err != nil {
		var exit *exec.ExitError
		detail := fmt.Sprintf("%s after %s", message, time.Since(start).Round(time.Millisecond))
		if errors.As(err, &exit) {
			s.traceLevel("WARN", "AUDIO", fmt.Sprintf("%s (watchdog exit %d)", detail, exit.ExitCode()))
		} else {
			s.traceFailure("AUDIO", detail, err)
		}
		if s.ctx.Err() == nil {
			s.async(func() { s.log("AUDIO", message) })
		}
	}
}

// Only copy fixed messages and HRESULTs into the debug log
var watchdogDiagnostic = regexp.MustCompile(`^(?:Core Audio (?:method [0-9]{1,2} failed|initialization failed|enumerator unavailable) \(0x[0-9a-f]{8}\)|another GSX mute request is still active|(?:read|write|clear) mute journal|mute journal has invalid JSON|mute watchdog pipe failed|matching audio session has no recoverable identity)$`)

// A journal error can include its filename or a fragment of its contents
func safeWatchdogDiagnostic(line string) string {
	reason, _, _ := strings.Cut(line, ":")
	if watchdogDiagnostic.MatchString(reason) {
		return reason
	}
	return ""
}

// Stream status while stdin remains open, so EOF also works on app exit
func (s *state) runWatchdog(ctx context.Context, restore bool, ready func()) error {
	if ctx.Err() != nil {
		return nil
	}
	cmd, err := watchdog(s.dir, restore)
	if err != nil {
		return err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer out.Close()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() {
		_ = in.Close()
	})
	defer stop()
	scan := bufio.NewScanner(out)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		switch {
		case line == "ready" && ready != nil:
			ready()
			ready = nil
		case line == "no-match":
			s.trace("AUDIO", "Mute watchdog found no matching Couatl/GSX audio session")
		case line == "matched":
			s.trace("AUDIO", "Mute watchdog confirmed a muted Couatl/GSX audio session")
		default:
			if detail := safeWatchdogDiagnostic(line); detail != "" {
				s.traceLevel("WARN", "AUDIO", "Mute watchdog: "+detail)
			}
		}
	}
	// A broken protocol stream must still give the child an EOF to restore
	_ = in.Close()
	_ = out.Close()
	err = errors.Join(scan.Err(), cmd.Wait())
	if err == nil && ready != nil && ctx.Err() == nil {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Revisit pending journals in case an audio endpoint was unavailable
func (s *state) restoreMute(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if _, err := os.Stat(filepath.Join(s.dir, "gsx-mute.json")); err == nil {
			if err := s.runWatchdog(ctx, true, nil); err != nil {
				s.traceFailure("AUDIO", "Could not restore Couatl/GSX audio", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
