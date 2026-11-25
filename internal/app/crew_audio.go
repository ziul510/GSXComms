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
	prefs             config.Values
	pcm               []byte
	rate              int
	cue, option       string
	voice, crewAccent string
	readout           string
	readoutEntries    int
	readoutGeneration uint64
	readoutCtx        context.Context
	preparing         *readoutPreparation
	location          *geo.Position
	priority, ordered bool
	dry               bool
}

// Hold the result off the playback goroutine until the reader has finished.
type readoutPreparation struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	pcm    []byte
	rate   int
}

func (s *state) randomizeVoice() {
	s.crewVoice = []string{"female", "male"}[rand.IntN(2)]
	s.log("SYS", "Crew voice selected: "+s.crewVoice)
}

func (s *state) cue(prefs config.Values, name, option string, interrupt bool) {
	s.enqueueCue(prefs, name, option, interrupt, false)
}

func (s *state) enqueueCue(prefs config.Values, name, option string, priority, stateChange bool) {
	prefs = prefs.Copy()
	// Use the latest saved accent settings even if the command started before the save
	for _, key := range []string{"force_accent", "fallback_accent", "installed_packs"} {
		prefs[key] = s.cfg[key]
	}
	s.queueAudio(playback{
		prefs: prefs, cue: strings.ToLower(name), option: option, voice: s.crewVoice, location: s.location.Load(), ordered: stateChange,
	}, priority)
}

func (p playback) protected() bool { return p.priority || p.ordered }

func (s *state) queueAudio(request playback, interrupt bool) bool {
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	// Settings can disable the reader while standalone synthesis is running.
	if s.ctx.Err() != nil || request.dry && request.readoutGeneration != s.readoutGeneration.Load() {
		return false
	}
	request.priority = interrupt
	const queueLimit = 16
	if interrupt {
		// Repeated pending errors only need one audible cue.
		if slices.ContainsFunc(s.audioQueue, func(p playback) bool { return p.priority && p.cue == request.cue && p.option == request.option }) {
			return true
		}
		if s.audioCancel != nil {
			s.audioCancel()
		}
		s.audioQueue = slices.DeleteFunc(s.audioQueue, func(p playback) bool { return !p.protected() })
		if len(s.audioQueue) == queueLimit {
			s.coalesceQueuedCrewCues("")
		}
		i := 0
		for i < len(s.audioQueue) && s.audioQueue[i].priority {
			i++
		}
		if len(s.audioQueue) == queueLimit {
			// Prefer dropping an ordered readout over losing the latest service state.
			drop := slices.IndexFunc(s.audioQueue, func(p playback) bool { return !p.priority && !p.crewState() })
			if drop < 0 {
				drop = 0
				i = max(0, i-1)
			}
			s.dropAudio(drop)
		}
		// Keep interrupts in arrival order and place them ahead of preset audio.
		s.audioQueue = slices.Insert(s.audioQueue, i, request)
	} else if len(s.audioQueue) < queueLimit {
		s.audioQueue = append(s.audioQueue, request)
	} else if !request.crewState() || !s.replaceQueuedCrewCue(request, queueLimit) {
		s.traceLevel("WARN", "AUDIO", "Playback skipped because the queue is full")
		s.async(func() { s.log("AUDIO", "Playback queue is full") })
		return false
	}
	select {
	case s.audioWake <- struct{}{}:
	default:
	}
	return true
}

// Cancel synthesis for a readout that is no longer queued.
func (s *state) dropAudio(index int) {
	if job := s.audioQueue[index].preparing; job != nil {
		job.cancel()
	}
	s.audioQueue = slices.Delete(s.audioQueue, index, index+1)
}

// Identify service transitions separately from ordered speech.
func (p playback) crewState() bool { return p.ordered && p.cue != "" }

// Keep only the latest pending state per service when the queue is full.
func (s *state) coalesceQueuedCrewCues(incoming string) {
	for i := len(s.audioQueue) - 1; i >= 0; i-- {
		p := s.audioQueue[i]
		// A later cue or the incoming state supersedes this transition.
		if p.crewState() && (p.cue == incoming || slices.ContainsFunc(s.audioQueue[i+1:], func(next playback) bool {
			return next.crewState() && next.cue == p.cue
		})) {
			s.dropAudio(i)
		}
	}
}

// Prefer fresh service state and keep the order of the cues that survive.
func (s *state) replaceQueuedCrewCue(request playback, limit int) bool {
	s.coalesceQueuedCrewCues(request.cue)
	if len(s.audioQueue) == limit {
		index := slices.IndexFunc(s.audioQueue, func(p playback) bool { return !p.protected() })
		// A queue full of ordered readouts still needs room for service state.
		if index < 0 {
			index = slices.IndexFunc(s.audioQueue, func(p playback) bool { return !p.priority && !p.crewState() })
		}
		if index < 0 {
			return false
		}
		s.dropAudio(index)
	}
	s.audioQueue = append(s.audioQueue, request)
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
		if next.dry && next.readoutGeneration != s.readoutGeneration.Load() {
			s.dropAudio(0)
			s.audioMu.Unlock()
			continue
		}
		if next.readout != "" {
			job := next.preparing
			if job == nil {
				// The queue retains preset order while urgent cues can move ahead.
				job = s.prepareReadout(next)
				s.audioQueue[0].preparing = job
			}
			select {
			case <-job.done:
				next.pcm, next.rate = job.pcm, job.rate
			default:
				if job.ctx.Err() == nil {
					s.audioMu.Unlock()
					select {
					case <-s.audioWake:
					case <-job.done:
					case <-job.ctx.Done():
					}
					continue
				}
			}
			if job.ctx.Err() != nil || len(next.pcm) == 0 {
				s.dropAudio(0)
				s.audioMu.Unlock()
				continue
			}
		}
		s.dropAudio(0)
		parent := s.ctx
		// Settings can cancel a dry readout after it has left the queue.
		if next.readoutCtx != nil {
			parent = next.readoutCtx
		}
		ctx, cancel := context.WithCancel(parent)
		// Only expose a cancel function for playback that an interrupt may cut short.
		if !next.protected() {
			s.audioCancel = cancel
		}
		s.audioMu.Unlock()

		if next.cue != "" {
			next.pcm, next.rate, next.crewAccent = s.regionalCue(next)
		}
		if ctx.Err() == nil {
			if next.cue != "" && len(next.pcm) == 0 {
				// Suppress repeated warnings for the same missing cue.
				if missing != next.cue {
					message := "No installed crew recording for " + next.cue
					s.async(func() { s.log("AUDIO", message) })
					s.traceLevel("WARN", "AUDIO", "No installed crew recording matched the cue")
				}
				missing = next.cue
			} else if err := s.output(ctx, next); err != nil && ctx.Err() == nil {
				s.async(func() { s.log("AUDIO", err.Error()) })
				s.traceFailure("AUDIO", "Windows audio playback failed", err)
			}
		}
		cancel()
		s.audioMu.Lock()
		s.audioCancel = nil
		s.audioMu.Unlock()
	}
}

// Start Kokoro outside playback and use the worker's synthesis deadline
func (s *state) prepareReadout(next playback) *readoutPreparation {
	parent := s.ctx
	if next.readoutCtx != nil {
		parent = next.readoutCtx
	}
	ctx, cancel := context.WithCancel(parent)
	job := &readoutPreparation{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(job.done)
		if ctx.Err() == nil && s.menuReaderReady() {
			job.pcm, job.rate = s.synthesizeReadout(ctx, next.readout, next.readoutEntries)
		}
	}()
	return job
}

var statefulServices = []string{"GPU", "JETWAY", "STAIRS"}

func (s *state) resetServices() {
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()
	clear(s.services)
}

// Service state is inferred from successful selections, just as in the legacy app
func (s *state) acknowledge(prefs config.Values, service, selected string, voice bool) {
	if slices.Contains(statefulServices, service) {
		s.servicesMu.Lock()
		defer s.servicesMu.Unlock()
		was := s.services[service]
		now, known := serviceConnected(was, selected)
		// Skip the cue if the label cannot tell us that the service changed state.
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

// Match the legacy transition order for GPU, jetway, and stairs
var serviceTransitions = []struct {
	words  []string
	target string
}{
	{[]string{"disconnect", "remove", "retract", "undock", "completed", "stop", "close", "release", "dismiss", "free"}, "disconnect"},
	{[]string{"operate"}, "toggle"},
	{[]string{"request", "connect", "call", "open"}, "connect"},
}

func serviceConnected(was bool, selected string) (connected, known bool) {
	selected = strings.Join(strings.FieldsFunc(strings.ToLower(selected), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}), " ")
	// Check disconnect terms before connect terms because the labels can contain both.
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
	workers sync.WaitGroup
	active  int
	window  *muteWindow
	timer   *time.Timer
	until   time.Time
}

type muteWindow struct {
	ready  chan struct{}
	cancel context.CancelFunc
}

// Hold the watchdog through overlapping commands and audio plus the grace period
func (s *state) muteCrew(ctx context.Context, prefs config.Values, grace time.Duration) (<-chan struct{}, func()) {
	if !config.Get[bool](prefs, "mute_gsx_audio_during_crew_voice") || ctx.Err() != nil {
		return nil, func() {}
	}
	return s.crewMute.hold(s.ctx, grace, s.runMute)
}

// Start the watchdog off the menu and playback paths
func (m *crewMute) hold(ctx context.Context, grace time.Duration, run func(context.Context, func())) (<-chan struct{}, func()) {
	m.Lock()
	defer m.Unlock()
	if ctx.Err() != nil {
		return nil, func() {}
	}
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.window == nil {
		watchCtx, cancel := context.WithCancel(ctx)
		window := &muteWindow{ready: make(chan struct{}), cancel: cancel}
		m.window = window
		m.workers.Add(1)
		go func() {
			defer m.workers.Done()
			defer cancel()
			ready := sync.OnceFunc(func() { close(window.ready) })
			run(watchCtx, ready)
			ready()
			m.Lock()
			defer m.Unlock()
			// A failed watchdog can be retried by the next hold
			if m.window == window {
				m.stop()
			}
		}()
	}
	m.active++
	return m.window.ready, func() {
		m.Lock()
		defer m.Unlock()
		m.active--
		// A playback release must preserve any later command deadline.
		until := time.Now().Add(grace)
		if until.After(m.until) {
			m.until = until
		}
		// Restore audio only after both the active holds and their grace periods end.
		if m.active == 0 && m.window != nil {
			var timer *time.Timer
			timer = time.AfterFunc(time.Until(m.until), func() {
				m.Lock()
				defer m.Unlock()
				// A newer speech window may have replaced this timer while it waited for the lock
				if m.timer == timer {
					m.stop()
				}
			})
			m.timer = timer
		}
	}
}

// Give stopping watchdogs time to restore audio or reach the forced stop.
func (m *crewMute) wait(ctx context.Context) {
	m.Lock()
	m.stop()
	m.Unlock()
	// Serialize with holds started before app cancellation, including retired watchdogs.
	done := make(chan struct{})
	go func() {
		m.workers.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
}

// The caller holds the mute lock when stopping the watchdog
func (m *crewMute) stop() {
	m.until = time.Time{}
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.window != nil {
		m.window.cancel()
		m.window = nil
	}
}
