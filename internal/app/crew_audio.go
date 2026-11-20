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
	prefs                 config.Values
	pcm                   []byte
	rate                  int
	cue, option           string
	voice                 string
	location              *geo.Position
	priority, stateChange bool
	dry                   bool
}

func (s *state) randomizeVoice() {
	s.crewVoice = []string{"female", "male"}[rand.IntN(2)]
	s.log("SYS", "Crew voice selected: "+s.crewVoice)
}

func (s *state) cue(prefs config.Values, name, option string, interrupt bool) {
	s.enqueueCue(prefs, name, option, interrupt, false)
}

func (s *state) enqueueCue(prefs config.Values, name, option string, priority, stateChange bool) {
	next := s.settingsDraft()
	forced := config.Get[string](next, "force_accent")
	prefs = prefs.Copy()
	prefs.Put("force_accent", forced)
	queued := s.queueAudio(playback{
		prefs: prefs, cue: strings.ToLower(name), option: option, voice: s.crewVoice, location: s.location.Load(), stateChange: stateChange,
	}, priority)
	// Consume the accent only when the interaction actually enters playback.
	if queued && forced != "" && forced != "Disabled" {
		next.Put("force_accent", "Disabled")
		s.save(next, nil)
	}
}

func (p playback) protected() bool { return p.priority || p.stateChange }

func (s *state) queueAudio(request playback, interrupt bool) bool {
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	if s.ctx.Err() != nil {
		return false
	}
	request.priority = interrupt
	if interrupt {
		if s.audioCancel != nil {
			s.audioCancel()
		}
		s.audioQueue = slices.DeleteFunc(s.audioQueue, func(p playback) bool { return !p.protected() })
		i := 0
		for i < len(s.audioQueue) && s.audioQueue[i].priority {
			i++
		}
		s.audioQueue = slices.Insert(s.audioQueue, i, request)
	} else if request.stateChange || len(s.audioQueue) < 16 {
		s.audioQueue = append(s.audioQueue, request)
	} else {
		s.async(func() { s.log("AUDIO", "Playback queue is full") })
		return false
	}
	select {
	case s.audioWake <- struct{}{}:
	default:
	}
	return true
}

func (s *state) playbackLoop() {
	var missing string
	for s.ctx.Err() == nil {
		s.audioMu.Lock()
		if len(s.audioQueue) == 0 {
			s.audioMu.Unlock()
			select {
			case <-s.audioWake:
			case <-s.ctx.Done():
				return
			}
			continue
		}
		next := s.audioQueue[0]
		s.audioQueue[0] = playback{}
		s.audioQueue = s.audioQueue[1:]
		ctx, cancel := context.WithCancel(s.ctx)
		if !next.protected() {
			s.audioCancel = cancel
		}
		s.audioMu.Unlock()

		if next.cue != "" {
			next.pcm, next.rate = s.regionalCue(next)
		}
		if ctx.Err() == nil {
			if next.cue != "" && len(next.pcm) == 0 {
				if missing != next.cue {
					message := "No installed crew recording for " + next.cue
					s.async(func() { s.log("AUDIO", message) })
					s.trace("AUDIO", "No installed crew recording matched the cue")
				}
				missing = next.cue
			} else if err := s.output(ctx, next); err != nil && ctx.Err() == nil {
				s.async(func() { s.log("AUDIO", err.Error()) })
				s.traceFailure("AUDIO", "Windows audio playback failed")
			}
		}
		cancel()
		s.audioMu.Lock()
		s.audioCancel = nil
		s.audioMu.Unlock()
	}
}

var statefulServices = []string{"GPU", "JETWAY", "STAIRS"}

func (s *state) resetServices() {
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()
	clear(s.services)
}

// Service state is inferred from successful selections, just as in the legacy app.
func (s *state) acknowledge(prefs config.Values, service, selected string, voice bool) {
	if slices.Contains(statefulServices, service) {
		s.servicesMu.Lock()
		defer s.servicesMu.Unlock()
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
				s.enqueueCue(prefs, service, action, false, true)
			}
		})
	} else if voice && service != "" {
		s.async(func() { s.cue(prefs, service, "", false) })
	}
}

// Python v4.1 uses this order for GPU, jetway, and stairs
var serviceTransitions = []struct {
	words  []string
	target string
}{
	{[]string{"disconnect", "remove", "retract", "completed", "stop", "close", "release", "dismiss", "free"}, "disconnect"},
	{[]string{"operate"}, "toggle"},
	{[]string{"request", "connect", "call", "open"}, "connect"},
}

func serviceConnected(was bool, selected string) (connected, known bool) {
	selected = strings.Join(strings.FieldsFunc(strings.ToLower(selected), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}), " ")
	for _, transition := range serviceTransitions {
		for _, word := range transition.words {
			if strings.Contains(selected, word) {
				return transition.target == "connect" || transition.target == "toggle" && !was, true
			}
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

// Commands and their audio can overlap. Keep one watchdog until both finish and the grace period expires.
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
				// A newer speech window may have replaced this timer while it waited for the lock.
				if m.timer == timer {
					m.stop()
				}
			})
			m.timer = timer
		}
	}
}

// The caller holds the mute lock when stopping the watchdog.
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
