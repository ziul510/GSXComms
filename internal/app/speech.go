// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-gui-org/go-gui/gui"
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
	if !s.speechReady {
		s.log("SYS", "Speech recognition unavailable or still loading")
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
		if volume := config.Get[float64](prefs, "volume_static"); volume > 0 && config.Get[bool](prefs, "enable_static") {
			go func() {
				pcm := make([]byte, 48000)
				mixRadio(pcm, 0, volume)
				_ = playPCMBuffer(capctx, config.Get[string](prefs, "output_device_name"), pcm, 24000, true)
			}()
		}
		// Voxtral uploads audio, so a missing or busy named mic must stop capture
		pcm, captureErr := capture(capctx, config.Get[string](prefs, "input_device_name"), config.Get[int](prefs, "max_recording_seconds"), localSpeech)
		if captureErr == nil && capctx.Err() == nil {
			// A full recording still waits for release so held PTT cannot transmit twice.
			s.async(func() { s.log("SYS", "Recording cap reached. Release PTT to transmit") })
			<-capctx.Done()
		}
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
		start := time.Now()
		transcript, err := s.transcribe(prefs, pcm)
		s.async(func() {
			s.processing = false
			s.ptt = "Idle"
			if config.Get[bool](s.cfg, "debug_mode") {
				s.debugLog(debugEvent{true, fmt.Sprintf("[STT] %s decoded %.2f seconds of audio in %s",
					provider, float64(len(pcm))/32000, time.Since(start).Round(time.Millisecond))})
			}
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
	s.sttMu.Lock()
	defer s.sttMu.Unlock()
	// Keep the model and hotwords stable between loading and transcribing this recording.
	if err := s.loadRecognizer(s.ctx, m, vocab); err != nil {
		return "", err
	}
	return s.recognizer.Transcribe(s.ctx, pcm)
}

func (s *state) loadRecognizer(ctx context.Context, model models.Model, vocab commands.Vocabulary) error {
	terms := vocab.Hotwords()
	signature := fmt.Sprintf("%s:%x", model.Revision, sha256.Sum256([]byte(strings.Join(terms, "\n"))))
	return s.recognizer.Load(ctx, signature, model.Paths(s.dir), terms)
}

func downloadModel(ctx context.Context, model models.Model, dir string, progress func(int64, int64)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stalled := errors.New("Download stalled. Check your connection and try again")
	// A stalled transfer can resume from its verified staging directory on retry.
	timer := time.AfterFunc(180*time.Second, func() { cancel(stalled) })
	defer timer.Stop()
	err := model.Download(ctx, dir, func(done, total int64) {
		timer.Reset(180 * time.Second)
		progress(done, total)
	})
	if errors.Is(context.Cause(ctx), stalled) {
		return stalled
	}
	return err
}

func (s *state) prepareSpeech(autoDownload bool) {
	if s.recognizer == nil {
		return
	}
	if s.speechLoadCancel != nil {
		s.speechLoadCancel()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.speechLoadCancel = cancel
	s.speechGeneration++
	generation := s.speechGeneration
	provider := config.Get[string](s.cfg, "speech_provider")
	s.speechReady, s.speechStatus, s.speechLoading = false, "Loading speech", provider
	s.log("SYS", "Loading speech recognition")
	model, local := models.Lookup(provider)
	download := local && !model.Ready(s.dir) && autoDownload && !s.speechAttempted[provider]
	if download {
		s.speechAttempted[provider] = true
		s.speechStatus = "Downloading model"
	}
	go func() {
		defer cancel()
		vocab, err := commands.Load(s.dir)
		if err == nil {
			switch {
			case provider == "mistral":
				var key string
				key, err = config.ReadKey()
				if err == nil && key == "" {
					err = errors.New("Mistral API key is missing")
				}
			case !local:
				err = errors.New("Selected speech provider is unavailable")
			default:
				if download {
					err = downloadModel(ctx, model, s.dir, func(done, total int64) {
						percent := int(done * 100 / max(1, total))
						s.async(func() {
							if generation == s.speechGeneration {
								s.speechStatus = fmt.Sprintf("Downloading %d%%", percent)
							}
						})
					})
				}
				if err == nil && !model.Ready(s.dir) {
					err = errors.New("Open Settings to download the selected speech model")
				}
				if err == nil {
					s.sttMu.Lock()
					err = s.loadRecognizer(ctx, model, vocab)
					s.sttMu.Unlock()
				}
			}
		}
		s.async(func() {
			if generation != s.speechGeneration {
				// A provider change invalidates startup results already queued for the window.
				return
			}
			s.speechLoading = ""
			s.speechReady, s.speechStatus = err == nil, ""
			if err != nil {
				s.speechStatus = "Speech unavailable"
				s.log("STT", err.Error())
			} else {
				s.log("STT", "Speech recognition ready ("+provider+")")
			}
			if s.speechChanged != nil {
				s.speechChanged(provider)
			}
			s.host.Broadcast(func(w *gui.Window) { w.InvalidateLayout() })
		})
	}()
}
