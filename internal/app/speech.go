// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/ziul510/GSXCommsGo/internal/commands"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/models"
	"github.com/ziul510/GSXCommsGo/internal/speech"
)

func (s *state) record(down bool) {
	// A release ends capture The collected audio still has to pass the speech gate
	if !down {
		s.pttHeld = false
		if s.recording != nil {
			s.recording()
		}
		return
	}
	// Ignore repeated key-down events while PTT is held
	if s.pttHeld {
		return
	}
	s.pttHeld = true
	// Only one capture or transcription may be active for the PTT control
	if s.recording != nil || s.processing || s.settingUpMistral {
		return
	}
	if !s.connected && !config.Get[bool](s.cfg, "simulation_mode") {
		return
	}
	// Check the current flight state before opening the microphone
	if s.transmissionBlock() != "" {
		return
	}
	// Keep the microphone and provider fixed for the life of this recording
	prefs := s.cfg.Copy()
	provider := config.Get[string](prefs, "speech_provider")
	localSpeech := provider != "mistral"
	// Local capture needs the selected pinned model on disk before recording can begin
	if localSpeech {
		model, ok := models.Lookup(provider)
		if !ok || !model.Ready(s.dir) {
			s.log("SYS", "Open Settings to download the selected speech model")
			return
		}
	}
	capctx, stop := context.WithCancel(s.ctx)
	s.recording = stop
	s.ptt = "Recording…"
	go func() {
		defer stop()
		// Voxtral uploads audio, so a missing or busy named mic must stop capture
		pcm, captureErr := capture(capctx, config.Get[string](prefs, "input_device_name"), config.Get[int](prefs, "max_recording_seconds"), localSpeech)
		if s.ctx.Err() != nil {
			return
		}
		// Skip recognition for empty, quiet, or failed captures
		usable := captureErr == nil && speech.Usable(pcm)
		s.async(func() {
			s.recording = nil
			// Move the UI from recording to transcription only if audio will be submitted
			s.processing = usable
			s.ptt = "Idle"
			if captureErr != nil {
				s.log("ERR", captureErr.Error())
			}
			if usable {
				s.ptt = "Transcribing…"
			}
		})
		if !usable {
			return
		}
		// PTT release stops capture Transcription still uses the app context
		transcript, err := s.transcribe(prefs, pcm)
		s.async(func() {
			s.processing = false
			s.ptt = "Idle"
			if err != nil {
				s.log("ERR", err.Error())
				return
			}
			if transcript == "" {
				s.log("SYS", "No speech recognized")
				return
			}
			// Recognized speech and typed commands share routing and the current speed gate
			s.submit(transcript)
		})
	}()
}

func (s *state) transcribe(prefs config.Values, pcm []byte) (string, error) {
	provider := config.Get[string](prefs, "speech_provider")
	vocab, err := commands.Load(s.dir)
	if err != nil {
		return "", err
	}
	// Only the explicitly selected cloud provider reads the credential and sends audio
	if provider == "mistral" {
		key, err := config.ReadKey()
		if err != nil {
			return "", err
		}
		// Send the vocabulary table so cloud transcription uses the same service words as routing
		return speech.Voxtral(s.ctx, key, config.Get[string](prefs, "mistral_model"), vocab.HotwordTable(), pcm)
	}
	m, ok := models.Lookup(provider)
	if !ok || !m.Ready(s.dir) {
		return "", fmt.Errorf("speech model is unavailable")
	}
	terms := vocab.Hotwords()
	// Hotword changes need a worker reload even when the model revision stays the same
	signature := fmt.Sprintf("%s:%x", m.Revision, sha256.Sum256([]byte(strings.Join(terms, "\n"))))
	if err := s.recognizer.Load(s.ctx, signature, m.Paths(s.dir), terms); err != nil {
		return "", err
	}
	return s.recognizer.Transcribe(s.ctx, pcm)
}
