// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"

	"github.com/ziul510/GSXCommsGo/internal/assets"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/geo"
)

// Each queued clip carries its own settings and either PCM or an embedded cue name
type playback struct {
	prefs       config.Values
	pcm         []byte
	rate        int
	cue, option string
	location    *geo.Position
}

func (s *state) cue(prefs config.Values, name, option string, high bool) {
	s.queueAudio(playback{prefs: prefs, cue: strings.ToLower(name), option: option, location: s.location.Load()}, high)
}

func (s *state) queueAudio(request playback, high bool) {
	// Service confirmations and other priority cues can use their own queue
	queue := s.audioQueue
	if high {
		queue = s.audioPriority
	}
	select {
	case queue <- request:
	case <-s.ctx.Done():
	// Drop a cue on overflow so the simulator or speech worker never waits for audio output
	default:
		s.async(func() { s.log("AUDIO", "Playback queue is full") })
	}
}

func (s *state) playbackLoop() {
	for s.ctx.Err() == nil {
		var next playback
		select {
		// Check priority first so an already waiting cue wins over normal playback
		case next = <-s.audioPriority:
		default:
			select {
			case next = <-s.audioPriority:
			case next = <-s.audioQueue:
			case <-s.ctx.Done():
				return
			}
		}
		// Decode bundled clips only when they reach playback Synthesized clips already have PCM
		if next.cue != "" {
			next.pcm, next.rate = s.regionalCue(next)
			if len(next.pcm) == 0 {
				pcm, err := loadCue(next.cue)
				if err != nil {
					continue
				}
				next.pcm, next.rate = pcm, 24000
			}
			if next.cue == "error" {
				// Keep the error cue's mute override local to that request
				next.prefs = next.prefs.Copy()
				next.prefs.Put("mute_gsx_audio_during_crew_voice", false)
			}
		}
		if err := s.output(next.prefs, next.pcm, next.rate); err != nil && s.ctx.Err() == nil {
			s.async(func() { s.log("AUDIO", err.Error()) })
		}
	}
}

// The embedded cue files contain gzip-compressed sample bytes with no audio header
func loadCue(name string) ([]byte, error) {
	raw, err := assets.Files.ReadFile("cues/" + name + ".pcm.gz")
	if err != nil {
		return nil, err
	}
	stream, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	// Keep decompressed cue reads bounded even if an asset is malformed
	return io.ReadAll(io.LimitReader(stream, 1<<20))
}
