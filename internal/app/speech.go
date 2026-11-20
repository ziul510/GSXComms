// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/commands"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/models"
	"github.com/ziul510/GSXCommsGo/internal/speech"
)

func (s *state) record(down bool) {
	if !down {
		s.pttHeld = false
		if s.recording != nil {
			s.recording()
		}
		return
	}
	if s.pttHeld {
		return
	}
	s.pttHeld = true
	if s.recording != nil || s.processing {
		return
	}
	if !s.connected && !config.Get[bool](s.cfg, "simulation_mode") {
		return
	}
	if s.transmissionBlock() != "" {
		return
	}
	prefs := s.cfg.Copy()
	provider := config.Get[string](prefs, "speech_provider")
	localSpeech := provider != "mistral"
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
		// Voxtral uploads audio, so a missing or busy named mic must stop capture.
		pcm, captureErr := capture(capctx, config.Get[string](prefs, "input_device_name"), config.Get[int](prefs, "max_recording_seconds"), localSpeech)
		if s.ctx.Err() != nil {
			return
		}
		usable := captureErr == nil && speech.Usable(pcm)
		s.async(func() {
			s.recording = nil
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
	if provider == "mistral" {
		key, err := config.ReadKey()
		if err != nil {
			return "", err
		}
		return speech.Voxtral(s.ctx, key, config.Get[string](prefs, "mistral_model"), vocab.HotwordTable(), pcm)
	}
	m, ok := models.Lookup(provider)
	if !ok || !m.Ready(s.dir) {
		return "", fmt.Errorf("speech model is unavailable")
	}
	terms := vocab.Hotwords()
	// Hotword changes need a worker reload even when the model revision stays the same.
	signature := fmt.Sprintf("%s:%x", m.Revision, sha256.Sum256([]byte(strings.Join(terms, "\n"))))
	if err := s.recognizer.Load(s.ctx, signature, m.Paths(s.dir), terms); err != nil {
		return "", err
	}
	return s.recognizer.Transcribe(s.ctx, pcm)
}

func (s *state) openSpeech(parent *gui.Window) {
	if s.speechOpen {
		return
	}
	s.speechOpen = true
	var cancel context.CancelFunc
	var message, key string
	var busy bool
	closing := func() {
		if cancel != nil {
			cancel()
		}
		s.speechOpen = false
	}
	labels := map[string]string{"parakeet": "Standard, Recommended (Parakeet v2 0.6b)", "orukeet": "Enhanced (Orukeet)", "kokoro": "Kokoro 82M menu reader"}
	s.dialog(gui.WindowCfg{Title: "Speech Recognition", Width: 530, Height: 630, MinWidth: 440, MinHeight: 450}, closing, func(w *gui.Window) gui.View {
		var body []gui.View
		unavailable := busy || cancel != nil
		for _, id := range []string{"parakeet", "orukeet", "kokoro"} {
			m, _ := models.Lookup(id)
			ready := m.Ready(s.dir)
			status := "Available for download"
			if ready {
				status = "Downloaded"
			}
			actions := gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, Spacing: gui.SpacingPx(8), SizeBorder: gui.NoBorder, Content: []gui.View{
				button("download-"+id, "Download", 90, true, unavailable || ready, func() {
					ctx, stop := context.WithCancel(s.ctx)
					cancel = stop
					message = "Checking available disk space…"
					go func() {
						defer stop()
						err := m.Download(ctx, s.dir, func(done, total int64) {
							w.QueueCommand(func(w *gui.Window) {
								message = fmt.Sprintf("%s — %.1f / %.1f MiB", labels[id], float64(done)/(1<<20), float64(total)/(1<<20))
								w.InvalidateLayout()
							})
						})
						w.QueueCommand(func(w *gui.Window) {
							cancel = nil
							if err != nil {
								message = err.Error()
							} else {
								message = labels[id] + " downloaded"
							}
							w.InvalidateLayout()
						})
					}()
				}),
				button("reload-"+id, "Reload", 72, false, unavailable || !ready, func() {
					busy = true
					go func() {
						if id == "kokoro" {
							s.reader.Reset()
						} else {
							s.recognizer.Reset()
						}
						w.QueueCommand(func(w *gui.Window) {
							busy = false
							message = "Model will reload on next transmission"
							w.InvalidateLayout()
						})
					}()
				}),
				button("delete-"+id, "Delete", 72, false, unavailable || !m.Cached(s.dir), func() {
					busy = true
					message = "Deleting…"
					go func() {
						if id == "kokoro" {
							s.reader.Reset()
						} else {
							s.recognizer.Reset()
						}
						err := m.Delete(s.dir)
						w.QueueCommand(func(w *gui.Window) {
							busy = false
							if err != nil {
								message = err.Error()
							} else {
								message = "Model deleted"
							}
							w.InvalidateLayout()
						})
					}()
				}),
			}})
			body = append(body, gui.Column(outlinedCard(card, gui.PadAll(12), 8,
				bold(labels[id], 13, ink), text(status, 11, muted), actions)))
		}
		body = append(body, gui.Column(outlinedCard(card, gui.PadAll(12), 8,
			bold("Voxtral Small (cloud)", 13, ink),
			wrapped("Voxtral sends recordings to the Mistral cloud when selected. Create an API key in your Mistral account, then enter it here and validate it before selecting Voxtral in Settings.", 12, muted),
			formInput(gui.InputCfg{ID: "voxtral-key", Label: "Mistral API key", Text: key, IsPassword: true, OnTextChanged: func(v string, _ gui.EventCtx) { key = v }}),
			button("save-key", "Validate and save key", 0, false, s.savingKey, func() {
				s.savingKey = true
				token := strings.TrimSpace(key)
				key = ""
				message = "Validating key…"
				go func() {
					err := speech.ValidateMistral(s.ctx, token)
					if err == nil {
						err = config.SaveKey(token)
					}
					s.async(func() {
						s.savingKey = false
						if err != nil {
							message = err.Error()
						} else {
							message = "Voxtral API key saved"
						}
						s.host.Broadcast(func(w *gui.Window) { w.InvalidateLayout() })
					})
				}()
			}))))
		return page("speech-scroll", []gui.View{bold("Speech Recognition", 18, ink)}, body, []gui.View{
			wrapped(message, 11, muted), spacer(), button("stop-download", "Cancel download", 0, false, cancel == nil, func() {
				if cancel != nil {
					cancel()
				}
			}),
			button("speech-close", "Close", 0, false, false, func() { closing(); w.Close(); parent.InvalidateLayout() }),
		})
	})
}
