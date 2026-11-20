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
	// Couatl can still be replacing the panel after a selection.
	if since := time.Since(s.menuActivity); since < 4*time.Second {
		timer := time.NewTimer(4*time.Second - since)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}
	}
	m, _ := models.Lookup("kokoro")
	if !m.Ready(s.dir) {
		log("ERR", "Kokoro menu-reader model is unavailable. Turn menu readout off and back on in Settings to retry the download")
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
		menu, err := session.OpenMenuPage(ctx)
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
		log("GSX", fmt.Sprintf("Reading %d GSX menu entries aloud (%s)", len(options), s.reader.Runtime()))
		// Queue readout audio behind waiting priority crew cues
		s.queueAudio(playback{prefs: prefs, pcm: pcm, rate: rate, dry: true}, false)
	}()
}

func (s *state) output(ctx context.Context, next playback) error {
	if !next.dry && next.cue != "error" {
		defer s.muteCrew(ctx, next.prefs)()
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
	pcm := bytes.Clone(next.pcm)
	mixRadio(pcm, gain, static, next.rate, channels)
	s.trace("AUDIO", fmt.Sprintf("Playing %d bytes at %d Hz with %d channel(s)", len(pcm), next.rate, channels))
	return playPCMBuffer(ctx, config.Get[string](next.prefs, "output_device_name"), config.Get[string](next.prefs, "output_device_id"), pcm, next.rate, channels, false)
}

// Match the legacy RMS gain limits without turning silence into noise.
func normalizationGain(pcm []byte) float64 {
	var sum float64
	for i := 0; i+1 < len(pcm); i += 2 {
		sample := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		sum += sample * sample
	}
	if sum == 0 {
		return 1
	}
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
