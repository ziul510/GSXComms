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
)

type playback struct {
	prefs config.Values
	pcm   []byte
	rate  int
	cue   string
}

func (s *state) cue(prefs config.Values, name string, high bool) {
	s.queueAudio(playback{prefs: prefs, cue: strings.ToLower(name), rate: 24000}, high)
}

func (s *state) queueAudio(request playback, high bool) {
	queue := s.audioQueue
	if high {
		queue = s.audioPriority
	}
	select {
	case queue <- request:
	case <-s.ctx.Done():
	default:
		s.async(func() { s.log("AUDIO", "Playback queue is full") })
	}
}

func (s *state) playbackLoop() {
	for s.ctx.Err() == nil {
		var next playback
		select {
		case next = <-s.audioPriority:
		default:
			select {
			case next = <-s.audioPriority:
			case next = <-s.audioQueue:
			case <-s.ctx.Done():
				return
			}
		}
		if next.cue != "" {
			pcm, err := loadCue(next.cue)
			if err != nil {
				continue
			}
			next.pcm = pcm
			if next.cue == "error" {
				next.prefs = next.prefs.Copy()
				next.prefs.Put("mute_gsx_audio_during_crew_voice", false)
			}
		}
		if err := s.output(next.prefs, next.pcm, next.rate); err != nil && s.ctx.Err() == nil {
			s.async(func() { s.log("AUDIO", err.Error()) })
		}
	}
}

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
	return io.ReadAll(io.LimitReader(stream, 1<<20))
}
