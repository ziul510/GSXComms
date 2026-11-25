// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/models"
	"github.com/ziul510/GSXCommsGo/internal/speech"
	"golang.org/x/sys/windows"
)

type localModelStatus struct {
	ready, cached bool
	message       string
}

type speechSetup struct {
	local          map[string]localModelStatus
	running        string
	cancel         context.CancelFunc
	step           int
	key            string
	showKey        bool
	hasKey         bool
	keyMessage     string
	providerError  string
	savingProvider bool
	tutorialDir    string
	images         []tutorialImage
}

func (setup *speechSetup) runLocal(s *state, w *gui.Window, id, operation string, then func(error)) {
	if setup.running != "" || s.speechLoading == id {
		return
	}
	m, _ := models.Lookup(id)
	setup.running = id
	downloading := operation == "Downloading"
	ctx := s.ctx
	var cancel context.CancelFunc
	if downloading {
		ctx, cancel = context.WithCancel(ctx)
		setup.cancel = cancel
	}
	status := setup.local[id]
	status.message = operation + "…"
	setup.local[id] = status
	go func() {
		var err error
		if downloading {
			err = downloadModel(ctx, m, s.dir, func(done, total int64) {
				w.QueueCommand(func(w *gui.Window) {
					// Ignore stale progress callbacks after the download ends or the user cancels it.
					if ctx.Err() != nil || setup.running != id {
						return
					}
					status := setup.local[id]
					status.message = fmt.Sprintf("Downloading %.1f / %.1f MiB", float64(done)/(1<<20), float64(total)/(1<<20))
					setup.local[id] = status
					w.InvalidateLayout()
				})
			})
		} else {
			err = m.Delete(s.dir)
		}
		next := localModelStatus{ready: m.Ready(s.dir), cached: m.Cached(s.dir)}
		s.async(func() {
			if cancel != nil {
				// A canceled download can leave usable files without changing the selection
				if ctx.Err() != nil {
					err = ctx.Err()
				}
				cancel()
			}
			if errors.Is(err, context.Canceled) {
				next.message = "Download canceled"
			} else if err != nil {
				next.message = err.Error()
			}
			setup.running, setup.cancel = "", nil
			setup.local[id] = next
			// Reload the recognizer only if the download belongs to the active provider.
			if err == nil && downloading && config.Get[string](s.cfg, "speech_provider") == id {
				s.prepareSpeech(false, false)
			}
			if !w.CloseRequested() {
				if then != nil {
					then(err)
				}
				w.InvalidateLayout()
			}
		})
	}()
}

// Persist the provider before advertising it as active or triggering model startup
func (setup *speechSetup) useProvider(s *state, w *gui.Window, provider string, onSaved func()) {
	if setup.savingProvider {
		return
	}
	setup.savingProvider, setup.providerError = true, ""
	s.saveSetting("speech_provider", provider, func(err error) {
		setup.savingProvider = false
		if err != nil {
			setup.providerError = "Could not select speech provider: " + err.Error()
		} else if onSaved != nil {
			onSaved()
		}
		if !w.CloseRequested() {
			w.InvalidateLayout()
		}
	})
}

// Identify startup downloads separately from recognizer initialization
func (s *state) speechDownloading() bool {
	return s.speechLoading != "" && strings.HasPrefix(s.speechStatus, "Downloading")
}

func (setup *speechSetup) localCard(s *state, w *gui.Window, id, label string, saving bool) gui.View {
	status := setup.local[id]
	inUse := config.Get[string](s.cfg, "speech_provider") == id
	loading := s.speechLoading == id
	busy := saving || setup.running != "" || setup.savingProvider || loading
	note := status.message
	if loading {
		note = s.speechStatus
	}
	var actions []gui.View
	switch {
	case setup.running == id && setup.cancel != nil:
		actions = append(actions, modelButton("download-"+id, "Cancel", 88, false, setup.cancel))
	case loading && s.speechDownloading():
		actions = append(actions, modelButton("download-"+id, "Cancel", 88, false, s.speechLoadCancel))
	case !status.ready:
		actions = append(actions, modelButton("download-"+id, "Download", 88, busy, func() {
			setup.runLocal(s, w, id, "Downloading", func(err error) {
				if err == nil && !inUse {
					setup.useProvider(s, w, id, nil)
				}
			})
		}))
	case inUse:
		actions = append(actions, text("✓ Active", settingsTextSize, accent))
		if s.speechStatus == "Speech unavailable" && !busy {
			actions = append(actions, modelButton("retry-"+id, "Reload", 64, false, func() {
				s.prepareSpeech(false, true)
			}))
		}
	default:
		actions = append(actions, modelButton("provider-"+id, "Use", 56, busy, func() {
			setup.useProvider(s, w, id, nil)
		}))
	}
	other := "parakeet"
	if id == other {
		other = "orukeet"
	}
	// Keep recovery for partial files and protect the last working local model
	if status.cached && setup.running != id && !loading && (!status.ready || (!inUse && setup.local[other].ready)) {
		actions = append(actions, modelButton("delete-"+id, "Delete", 68, busy, func() {
			setup.runLocal(s, w, id, "Deleting", nil)
		}))
	}
	return speechCard(label, note, actions...)
}

func modelButton(id, label string, width float32, disabled bool, click func()) gui.View {
	control := sizedButtonConfig(id, label, width, 11, label == "Use", disabled, click)
	control.Height, control.Radius = 30, gui.RadiusPx(3)
	control.HAlign = gui.Some(gui.HAlignCenter)
	control.A11YLabel = label
	switch label {
	case "Use":
		control.Content = []gui.View{text(label, 11, bg)}
	// Give deletion a distinct color because it removes local model files.
	case "Delete":
		control.Colors.Base, control.Colors.Hover = elevated, panel
		control.Content = []gui.View{text(label, 11, red)}
	default:
		control.SizeBorder = gui.BorderPx(1)
		control.Colors.Border, control.Colors.BorderFocus = elevated, accent
	}
	return gui.Button(control)
}

func speechCard(label, note string, actions ...gui.View) gui.View {
	row := gui.Row(gui.ContainerCfg{
		Sizing: gui.FillFixed, Height: 30, Padding: gui.NoPadding, SizeBorder: gui.NoBorder,
		Spacing: gui.SpacingPx(4), VAlign: gui.VAlignMiddle,
		Content: append([]gui.View{bold(label, settingsTextSize, ink), spacer()}, actions...),
	})
	content := []gui.View{row}
	// Keep recovery messages below the actions so long errors do not crowd the buttons.
	if note != "" {
		content = append(content, wrapped(note, 11, muted))
	}
	return gui.Column(outlinedCard(card, gui.PadVH(8, 10), 4, content...))
}

const mistralWarning = "When you use Mistral, GSXComms sends your voice recordings to Mistral for transcription. Mistral may use them to train AI models, depending on your plan and privacy settings."

var mistralSteps = []string{
	"Open Mistral Studio and sign in or create an account with API access.",
	"In Mistral Studio, choose API Keys.",
	"Select Create new key.",
	"Name the key GSXComms, choose an expiration date and workspace, then select Create new key.",
	"Copy the key before closing the dialog. Mistral shows the full key only once.",
}

type tutorialImage struct {
	path          string
	width, height float32
}

func (setup *speechSetup) mistralCard(s *state, w *gui.Window, saving bool) gui.View {
	inUse := config.Get[string](s.cfg, "speech_provider") == "mistral"
	open := func() {
		setup.step = 1
		setup.providerError = ""
		s.settingUpMistral = true
		// Read image dimensions once so screenshots can scale without distorting
		if setup.images == nil {
			setup.images = make([]tutorialImage, len(mistralSteps))
			for i := range setup.images {
				path := filepath.Join(setup.tutorialDir, fmt.Sprintf("step%d.png", i+1))
				file, err := os.Open(path)
				if err != nil {
					continue
				}
				size, _, err := image.DecodeConfig(file)
				file.Close()
				if err == nil && size.Width > 0 && size.Height > 0 {
					setup.images[i] = tutorialImage{path, float32(size.Width), float32(size.Height)}
				}
			}
		}
	}
	busy := saving || setup.savingProvider || setup.running != ""
	const label = "Multilingual (Voxtral Small, cloud)"
	if inUse {
		return speechCard(label, s.speechStatus, text("✓ Active", settingsTextSize, accent),
			modelButton("provider-mistral", "Set up", 64, busy, open))
	}
	return speechCard(label, "", modelButton("provider-mistral", "Use", 56, busy, func() {
		if setup.hasKey {
			setup.useProvider(s, w, "mistral", nil)
		} else {
			open()
		}
	}))
}

func (setup *speechSetup) mistralPage(s *state, w *gui.Window) gui.View {
	header := []gui.View{bold("Set up Mistral", 20, ink)}
	leave := func() {
		setup.step, setup.key, setup.keyMessage, setup.showKey = 0, "", "", false
		s.settingUpMistral = false
	}
	busy := s.savingKey || setup.savingProvider
	back := button("mistral-back", "Back to Settings", 0, false, busy, leave)
	if setup.step <= len(mistralSteps) {
		i := setup.step - 1
		header = append(header, wrapped(fmt.Sprintf("Step %d of %d", setup.step, len(mistralSteps)), 13, muted))
		body := []gui.View{wrapped(mistralSteps[i], 15, ink)}
		if i == 0 {
			body = append(body, wrapped(mistralWarning, 14, orange), button("mistral-studio", "Open Mistral Studio", 0, false, false, func() {
				err := windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr("https://console.mistral.ai/"), nil, nil, windows.SW_SHOWNORMAL)
				if err != nil {
					setup.keyMessage = "Open console.mistral.ai in your browser."
				}
			}))
			if setup.keyMessage != "" {
				body = append(body, wrapped(setup.keyMessage, 13, muted))
			}
		}
		if screenshot := setup.images[i]; screenshot.path != "" {
			width, _ := w.WindowSize()
			scale := min(1, float32(max(1, width-64))/screenshot.width, 420/screenshot.height)
			body = append(body, gui.Image(gui.ImageCfg{
				ID: "mistral-screenshot", Src: screenshot.path, Width: screenshot.width * scale, Height: screenshot.height * scale,
				A11YCfg: gui.A11YCfg{A11YLabel: mistralSteps[i]},
			}))
		}
		nextLabel := "Next"
		if setup.step == len(mistralSteps) {
			nextLabel = "Enter API key"
		}
		return page("mistral-tutorial", header, body, []gui.View{back, spacer(),
			button("mistral-previous", "Previous", 0, false, i == 0, func() { setup.step--; w.ScrollVerticalToPct("mistral-tutorial", 0) }),
			button("mistral-next", nextLabel, 0, true, false, func() {
				setup.step++
				setup.keyMessage = ""
				w.ScrollVerticalToPct("mistral-tutorial", 0)
			}),
		})
	}
	useMistral := func() {
		setup.keyMessage = "Saving speech provider…"
		setup.useProvider(s, w, "mistral", func() {
			// Recheck a previously selected provider when the user supplies its missing key
			if config.Get[string](s.cfg, "speech_provider") == "mistral" && !s.speechReady && s.speechLoading == "" {
				s.prepareSpeech(false, false)
			}
			// A closed window's save must not dismiss a later tutorial
			if !w.CloseRequested() {
				leave()
			}
		})
	}
	body := []gui.View{
		bold("Voice privacy", 16, ink), wrapped(mistralWarning, 14, orange),
		wrapped("Paste your key below. Windows Credential Manager stores it after validation.", 14, muted),
		formInput(gui.InputCfg{ID: "voxtral-key", Label: "Mistral API key", Text: setup.key, IsPassword: !setup.showKey, Disabled: busy,
			OnTextChanged: func(v string, _ gui.EventCtx) { setup.key = v; w.InvalidateLayout() }}),
		toggleControl("show-mistral-key", "Show API key", settingsTextSize, setup.showKey, func(ctx gui.EventCtx) {
			setup.showKey = !setup.showKey
			ctx.Consume()
			w.InvalidateLayout()
		}),
		button("save-key", "Validate key and select Mistral", 0, true, busy || strings.TrimSpace(setup.key) == "", func() {
			token := strings.TrimSpace(setup.key)
			setup.key, setup.keyMessage, s.savingKey = "", "Validating key…", true
			go func() {
				err := speech.ValidateMistral(s.ctx, token)
				if err == nil {
					if saveErr := config.SaveKey(token); saveErr != nil {
						err = fmt.Errorf("Could not save key in Windows Credential Manager: %w", saveErr)
					}
				}
				s.async(func() {
					s.savingKey = false
					if err != nil {
						setup.keyMessage = err.Error()
					} else {
						setup.hasKey = true
						s.setLogSecret(token)
						if setup.step > 0 {
							useMistral()
						}
					}
					s.host.Broadcast(func(w *gui.Window) { w.InvalidateLayout() })
				})
			}()
		}),
	}
	if setup.hasKey {
		body = append(body, button("use-saved-key", "Select Mistral with saved key", 0, false, busy, useMistral))
	}
	if setup.providerError != "" {
		body = append(body, wrapped(setup.providerError, 13, red))
	} else if setup.keyMessage != "" {
		body = append(body, wrapped(setup.keyMessage, 13, muted))
	}
	return page("mistral-key", header, body, []gui.View{back, spacer(),
		button("mistral-last-step", "Previous", 0, false, busy, func() { setup.step--; setup.key = ""; setup.keyMessage = "" }),
	})
}
