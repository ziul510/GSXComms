// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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
	if !config.Get[bool](prefs, "read_gsx_menus_aloud_after_keyword") {
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

func (s *state) sayMenu(ctx context.Context, session *gsx.Session, prefs config.Values) {
	log := func(sender, note string) { s.async(func() { s.log(sender, note) }) }
	if !session.Connected() {
		log("SYS", "SimConnect handshake required to read the GSX menu")
		return
	}
	m, _ := models.Lookup("kokoro")
	if !m.Ready(s.dir) {
		log("ERR", "Kokoro menu-reader model is unavailable; open Speech model setup to download it")
		return
	}
	select {
	// Reserve the reader through synthesis so repeated requests don't pile up
	case s.speaking <- struct{}{}:
	default:
		log("SYS", "GSX menu readout is already in progress")
		return
	}
	// Reuse the menu if it is already populated Open a temporary one only when needed
	options := session.ReadMenu().Options
	if len(options) == 0 {
		if err := session.Open(ctx); err != nil {
			<-s.speaking
			log("ERR", err.Error())
			return
		}
		menu, err := session.WaitMenu(ctx)
		// Release the menu opened for readout before starting the slower speech work
		_ = session.Release(ctx)
		if err != nil {
			<-s.speaking
			log("ERR", err.Error())
			return
		}
		options = menu.Options
	}
	text := formatReadout(options)
	// Synthesizing speech is slower than GSX polling, so give the simulator queue back now
	go func() {
		defer func() { <-s.speaking }()
		if err := s.reader.Load(s.ctx, m.Revision, m.Paths(s.dir), nil); err != nil {
			log("ERR", "GSX menu readout failed: "+err.Error())
			return
		}
		pcm, rate, err := s.reader.Synthesize(s.ctx, text)
		if err != nil {
			log("ERR", "GSX menu readout failed: "+err.Error())
			return
		}
		log("GSX", fmt.Sprintf("Reading %d GSX menu entries aloud", len(options)))
		// Queue readout audio behind waiting priority crew cues
		s.queueAudio(playback{prefs: prefs, pcm: pcm, rate: rate}, false)
	}()
}

func (s *state) output(prefs config.Values, pcm []byte, rate int) error {
	// Apply separate gains to voice and static before playback
	change := config.Get[float64](prefs, "volume_voice")
	static := 0.0
	if config.Get[bool](prefs, "enable_static") {
		static = config.Get[float64](prefs, "volume_static")
	}
	prev := 0.0
	for i := 0; i+1 < len(pcm); i += 2 {
		noise := (rand.Float64()*2 - 1) * 1300
		sample := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		// Use the change in noise samples to give the static a sharper sound
		combined := sample*change + (noise-prev)*static
		// Clamp the mixed sample before converting back to PCM16
		binary.LittleEndian.PutUint16(pcm[i:], uint16(int16(max(-32768, min(32767, combined)))))
		prev = noise
	}
	// Request mute only for the duration of this clip and still play if setup fails
	if config.Get[bool](prefs, "mute_gsx_audio_during_crew_voice") {
		if release, err := startMute(s.ctx, s.dir); err == nil {
			defer release()
		} else {
			s.async(func() { s.log("SYS", "Could not mute Couatl/GSX audio") })
		}
	}
	return playPCM(s.ctx, config.Get[string](prefs, "output_device_name"), pcm, rate)
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
	cmd.Stderr = io.Discard
	return cmd, nil
}

func startMute(ctx context.Context, dir string) (func(), error) {
	cmd, err := watchdog(dir, false)
	if err != nil {
		return nil, err
	}
	// Hold stdin open for playback Use stdout for the watchdog's ready message
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, err
	}
	done := make(chan bool, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		done <- strings.TrimSpace(line) == "ready"
	}()
	release := func() {
		// The watchdog uses stdin EOF to restore audio even after the app crashes
		_ = in.Close()
		go cmd.Wait()
	}
	t := time.NewTimer(2500 * time.Millisecond)
	defer t.Stop()
	// Start muted playback only after acquisition, within the wait budget
	select {
	case ready := <-done:
		if ready {
			return release, nil
		}
	case <-t.C:
	case <-ctx.Done():
	}
	release()
	return nil, errors.New("GSX audio watchdog unavailable")
}

func restoreMute(ctx context.Context, dir string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		// Retry recovery because an audio device may have been absent on startup
		if _, err := os.Stat(filepath.Join(dir, "gsx-mute.json")); err == nil {
			if cmd, err := watchdog(dir, true); err == nil {
				_ = cmd.Run()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
