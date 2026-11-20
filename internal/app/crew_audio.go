// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/geo"
)

type playback struct {
	prefs       config.Values
	pcm         []byte
	rate        int
	cue, option string
	voice       string
	location    *geo.Position
	generation  uint64
	dry         bool
}

func (s *state) randomizeVoice() {
	s.crewVoice = []string{"female", "male"}[rand.IntN(2)]
	s.log("SYS", "Crew voice selected: "+s.crewVoice)
}

func (s *state) cue(prefs config.Values, name, option string, interrupt bool) {
	next := s.settingsDraft()
	forced := config.Get[string](next, "force_accent")
	prefs = prefs.Copy()
	prefs.Put("force_accent", forced)
	if forced != "" && forced != "Disabled" {
		next.Put("force_accent", "Disabled")
		s.save(next, nil)
	}
	s.queueAudio(playback{
		prefs: prefs, cue: strings.ToLower(name), option: option, voice: s.crewVoice, location: s.location.Load(),
	}, interrupt)
}

func (s *state) queueAudio(request playback, interrupt bool) {
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	if interrupt {
		s.audioGeneration++
		if s.audioCancel != nil {
			s.audioCancel()
		}
		for len(s.audioQueue) > 0 {
			select {
			case <-s.audioQueue:
			default:
			}
		}
	}
	request.generation = s.audioGeneration
	select {
	case s.audioQueue <- request:
	case <-s.ctx.Done():
	// Drop a cue on overflow so the simulator or speech worker never waits for audio output
	default:
		s.async(func() { s.log("AUDIO", "Playback queue is full") })
	}
}

func (s *state) playbackLoop() {
	var missing string
	for s.ctx.Err() == nil {
		var next playback
		select {
		case next = <-s.audioQueue:
		case <-s.ctx.Done():
			return
		}
		if next.cue != "" {
			next.pcm, next.rate = s.regionalCue(next)
			if len(next.pcm) == 0 {
				if missing != next.cue {
					message := "No installed crew recording for " + next.cue
					s.async(func() { s.log("AUDIO", message) })
				}
				missing = next.cue
				continue
			}
		}
		s.audioMu.Lock()
		if next.generation != s.audioGeneration {
			s.audioMu.Unlock()
			continue
		}
		ctx, cancel := context.WithCancel(s.ctx)
		s.audioCancel = cancel
		s.audioMu.Unlock()
		err := s.output(ctx, next)
		if err != nil && ctx.Err() == nil {
			s.async(func() { s.log("AUDIO", err.Error()) })
		}
		cancel()
		s.audioMu.Lock()
		s.audioCancel = nil
		s.audioMu.Unlock()
	}
}

var statefulServices = []string{"GPU", "JETWAY", "STAIRS"}

// Service state is inferred from successful selections, just as in the legacy app.
func (s *state) acknowledge(prefs config.Values, service, selected string, voice bool) {
	if slices.Contains(statefulServices, service) {
		was := s.services[service]
		now, known := serviceConnected(was, selected)
		if !known || now == was {
			return
		}
		s.services[service] = now
		action := "disconnect"
		if now {
			action = "connect"
		}
		s.async(func() {
			s.log("GSX", fmt.Sprintf("%s %sing", service, action))
			if voice {
				s.cue(prefs, service, action, false)
			}
		})
	} else if voice && service != "" {
		s.async(func() { s.cue(prefs, service, "", false) })
	}
}

func serviceConnected(was bool, selected string) (connected, known bool) {
	selected = strings.ToLower(selected)
	for _, word := range []string{"disconnect", "remove", "retract", "undock", "completed", "stop", "close", "release", "dismiss", "free"} {
		if strings.Contains(selected, word) {
			return false, true
		}
	}
	if strings.Contains(selected, "operate") || strings.Contains(selected, "operating") {
		return !was, true
	}
	for _, word := range []string{"request", "connect", "call", "open"} {
		if strings.Contains(selected, word) {
			return true, true
		}
	}
	return was, false
}

type crewMute struct {
	sync.Mutex
	active  int
	release func()
	timer   *time.Timer
}

func (s *state) muteCrew(ctx context.Context, prefs config.Values) func() {
	if !config.Get[bool](prefs, "mute_gsx_audio_during_crew_voice") || ctx.Err() != nil {
		return func() {}
	}
	m := &s.crewMute
	m.Lock()
	defer m.Unlock()
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.release == nil {
		release, err := startMute(ctx, s.dir)
		if err != nil {
			s.async(func() { s.log("AUDIO", "Could not mute Couatl/GSX audio") })
			return func() {}
		}
		m.release = release
	}
	m.active++
	return func() {
		m.Lock()
		defer m.Unlock()
		m.active--
		if m.active == 0 && m.release != nil {
			var timer *time.Timer
			timer = time.AfterFunc(3950*time.Millisecond, func() {
				m.Lock()
				defer m.Unlock()
				if m.timer == timer {
					m.stop()
				}
			})
			m.timer = timer
		}
	}
}

func (m *crewMute) stop() {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.release != nil {
		m.release()
		m.release = nil
	}
}
