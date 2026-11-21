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

// Share PTT readiness between the status row and the microphone gate
func (s *state) pttAvailable() bool {
	return s.pttBlock() == ""
}

func (s *state) pttBlock() string {
	switch {
	case s.settingUpMistral:
		return "Mistral setup is open"
	case !s.speechReady:
		return "Speech recognition unavailable or still loading"
	}
	simulation := config.Get[bool](s.cfg, "simulation_mode")
	if !simulation {
		if !s.connected {
			return "SimConnect disconnected"
		}
		if s.abovePTTAltitude.Load() {
			return "Aircraft above 10,000 ft"
		}
	}
	return s.speedBlock(simulation, config.Get[bool](s.cfg, "ignore_speed_restriction"))
}

func (s *state) record(down bool) {
	// A release ends capture The collected audio still has to pass the speech gate
	if !down {
		s.pttHeld = false
		s.heldHIDPath = ""
		if s.recording != nil {
			s.recording()
		}
		return
	}
	// Ignore repeated key press events while PTT is held
	if s.pttHeld {
		return
	}
	s.pttHeld = true
	// Only one capture or transcription may be active for the PTT control
	if s.recording != nil || s.processing {
		s.trace("PTT", "Press ignored while capture or transcription is active")
		return
	}
	// Check the current flight state before opening the microphone
	if blocked := s.pttBlock(); blocked != "" {
		s.traceLevel("WARN", "PTT", "Capture blocked: "+blocked)
		if !s.speechReady && !s.settingUpMistral {
			s.log("SYS", blocked)
		}
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
			s.traceLevel("WARN", "PTT", "Capture blocked: selected local speech model unavailable")
			s.log("SYS", "Open Settings to download the selected speech model")
			return
		}
	}
	capctx, stop := context.WithCancel(s.ctx)
	s.recording = stop
	s.ptt = "LISTENING"
	s.trace("PTT", fmt.Sprintf("Capture started | provider=%s, local mic fallback=%t", debugProvider(provider), localSpeech))
	go func() {
		defer stop()
		if volume := config.Get[float64](prefs, "volume_static"); volume > 0 && config.Get[bool](prefs, "enable_static") {
			go func() {
				if !waitForPTTStatic(capctx) {
					return
				}
				pcm := make([]byte, len(radioStatic())*2)
				mixRadio(pcm, 0, volume, staticRate, 1)
				_ = playPCMBuffer(capctx, config.Get[string](prefs, "output_device_name"), config.Get[string](prefs, "output_device_id"), pcm, staticRate, 1, true)
			}()
		}
		// Voxtral uploads audio, so a missing or busy named mic must stop capture
		pcm, captureErr := capture(capctx, config.Get[string](prefs, "input_device_name"), config.Get[string](prefs, "input_device_id"),
			config.Get[int](prefs, "max_recording_seconds"), localSpeech, func() {
				s.traceLevel("WARN", "AUDIO", "Selected microphone unavailable. Using the Windows default")
				s.async(func() {
					const message = "Selected microphone unavailable. Using the Windows default for this recording."
					s.log("AUDIO", message)
					s.main.Toast(gui.ToastCfg{Title: message})
				})
			})
		if captureErr == nil && capctx.Err() == nil {
			s.trace("PTT", "Recording cap reached. Waiting for release")
			// A full recording still waits for release so held PTT cannot transmit twice
			s.async(func() { s.log("SYS", "Recording cap reached. Release PTT to transmit") })
			<-capctx.Done()
		}
		if s.ctx.Err() != nil {
			return
		}
		// Skip recognition for empty, quiet, or failed captures
		usable := captureErr == nil && speech.Usable(pcm)
		audioSeconds := float64(len(pcm)) / 32000 // 16 kHz mono PCM16
		if captureErr != nil {
			s.traceFailure("AUDIO", "Microphone capture failed", captureErr)
		} else if !usable {
			s.trace("PTT", fmt.Sprintf("Capture skipped: %.2fs of audio, too short or quiet", audioSeconds))
		}
		s.async(func() {
			s.recording = nil
			// Move the UI from recording to transcription only if audio will be submitted
			s.processing = usable
			s.ptt = "Idle"
			if captureErr != nil {
				s.log("ERR", captureErr.Error())
			}
			if usable {
				s.ptt = "PROCESSING"
			}
		})
		if !usable {
			return
		}
		// PTT release stops capture Transcription still uses the app context
		start := time.Now()
		transcript, err := s.transcribe(prefs, pcm)
		elapsed := time.Since(start).Round(time.Millisecond)
		providerName := debugProvider(provider)
		switch {
		case err != nil:
			s.traceFailure("STT", fmt.Sprintf("%s transcription failed after %s (%.2fs of audio)", providerName, elapsed, audioSeconds), err)
		case transcript == "":
			s.trace("STT", fmt.Sprintf("%s detected no speech in %.2fs of audio (%s)", providerName, audioSeconds, elapsed))
		default:
			s.trace("STT", fmt.Sprintf("%s transcribed %.2fs of audio in %s", providerName, audioSeconds, elapsed))
		}
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

func waitForPTTStatic(ctx context.Context) bool {
	timer := time.NewTimer(275 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		// A release can arrive before the goroutine handles the timer
		return ctx.Err() == nil
	}
}

func (s *state) transcribe(prefs config.Values, pcm []byte) (string, error) {
	provider := config.Get[string](prefs, "speech_provider")
	// Only the explicitly selected cloud provider reads the credential and sends audio
	if provider == "mistral" {
		pcm, err := s.recognizer.FilterSpeech(s.ctx, pcm)
		if err != nil || len(pcm) == 0 {
			return "", err
		}
		key, err := config.ReadKey()
		if err != nil {
			return "", err
		}
		return speech.Voxtral(s.ctx, key, config.Get[string](prefs, "mistral_model"), pcm)
	}
	m, ok := models.Lookup(provider)
	if !ok || !m.Ready(s.dir) {
		return "", fmt.Errorf("speech model is unavailable")
	}
	s.sttMu.Lock()
	defer s.sttMu.Unlock()
	// Keep the model and hotwords stable between loading and transcribing this recording
	if err := s.loadRecognizer(s.ctx, m, s.loadVocabulary()); err != nil {
		return "", err
	}
	text, err := s.recognizer.Transcribe(s.ctx, pcm)
	return commands.CorrectLocalTranscript(text), err
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
	// A stalled transfer can resume from its verified staging directory on retry
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

func (s *state) prepareSpeech(autoDownload, reload bool) {
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
	s.publishTransmissionPolicy()
	s.log("SYS", "Loading speech recognition")
	model, local := models.Lookup(provider)
	download := local && !model.Ready(s.dir) && autoDownload && !s.speechAttempted[provider]
	if download {
		s.speechAttempted[provider] = true
		s.speechStatus = "Downloading model"
	}
	providerName := debugProvider(provider)
	s.trace("STT", fmt.Sprintf("Preparing %s | download=%t, reload=%t", providerName, download, reload))
	go func() {
		defer cancel()
		start := time.Now()
		vocab := s.loadVocabulary()
		var err error
		stage := "provider selection"
		switch {
		case provider == "mistral":
			stage = "Mistral credential check"
			var key string
			key, err = config.ReadKey()
			if err == nil && key == "" {
				stage = "missing Mistral API key"
				err = errors.New("Mistral API key is missing")
			}
		case !local:
			err = errors.New("Selected speech provider is unavailable")
		default:
			if download {
				stage = "model download"
				err = downloadModel(ctx, model, s.dir, func(done, total int64) {
					percent := int(done * 100 / max(1, total))
					s.async(func() {
						if generation == s.speechGeneration {
							s.speechStatus = fmt.Sprintf("Downloading %d%%", percent)
						}
					})
				})
				if err == nil {
					s.async(func() {
						if generation == s.speechGeneration {
							s.speechStatus = "Loading speech"
							if s.speechChanged != nil {
								s.speechChanged(provider)
							}
							s.host.Broadcast(func(w *gui.Window) { w.InvalidateLayout() })
						}
					})
				}
			}
			if err == nil && !model.Ready(s.dir) {
				stage = "model files unavailable"
				err = errors.New("Open Settings to download the selected speech model")
			}
			if err == nil {
				stage = "recognizer worker load"
				s.sttMu.Lock()
				if err = ctx.Err(); err == nil {
					if reload {
						s.recognizer.Reset()
					}
					err = s.loadRecognizer(ctx, model, vocab)
				}
				s.sttMu.Unlock()
			}
		}
		elapsed := time.Since(start).Round(time.Millisecond)
		s.async(func() {
			if generation != s.speechGeneration {
				// A provider change invalidates startup results already queued for the window
				return
			}
			s.speechLoading = ""
			s.speechReady, s.speechStatus = err == nil, ""
			s.publishTransmissionPolicy()
			if err != nil {
				s.traceFailure("STT", fmt.Sprintf("%s setup stopped at %s after %s", providerName, stage, elapsed), err)
				s.speechStatus = "Speech unavailable"
				s.log("STT", err.Error())
			} else {
				s.trace("STT", fmt.Sprintf("%s ready after %s", providerName, elapsed))
				s.log("STT", "Speech recognition ready ("+provider+")")
			}
			if s.speechChanged != nil {
				s.speechChanged(provider)
			}
			s.host.Broadcast(func(w *gui.Window) { w.InvalidateLayout() })
		})
	}()
}
