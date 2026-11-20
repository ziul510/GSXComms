// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
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
	ctx, cancel := context.WithCancel(s.ctx)
	setup.running, setup.cancel = id, cancel
	status := setup.local[id]
	status.message = operation + "…"
	setup.local[id] = status
	go func() {
		defer cancel()
		var err error
		var result string
		if operation == "Downloading" {
			err = downloadModel(ctx, m, s.dir, func(done, total int64) {
				w.QueueCommand(func(w *gui.Window) {
					status := setup.local[id]
					status.message = fmt.Sprintf("Downloading %.1f / %.1f MiB", float64(done)/(1<<20), float64(total)/(1<<20))
					setup.local[id] = status
					w.InvalidateLayout()
				})
			})
			result = "Downloaded"
		} else {
			err = m.Delete(s.dir)
			result = "Model deleted"
		}
		if err != nil {
			result = err.Error()
		}
		next := localModelStatus{ready: m.Ready(s.dir), cached: m.Cached(s.dir), message: result}
		s.async(func() {
			setup.running, setup.cancel = "", nil
			setup.local[id] = next
			if err == nil && operation == "Downloading" && config.Get[string](s.cfg, "speech_provider") == id {
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

// Persist the provider before advertising it as active or triggering model startup.
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

func (setup *speechSetup) localCard(s *state, w *gui.Window, id, label string) gui.View {
	status := setup.local[id]
	note := "Not downloaded"
	if status.ready {
		note = "Downloaded"
	}
	inUse := config.Get[string](s.cfg, "speech_provider") == id
	busy := setup.running != "" || setup.savingProvider || s.speechLoading == id
	action := func(id, label string, primary, disabled bool, click func()) gui.View {
		control := buttonConfig(id, label, 0, primary, disabled, click)
		control.Height = 30
		return gui.Button(control)
	}
	var actions []gui.View
	if id != "kokoro" && !status.ready {
		actions = append(actions, action("download-"+id, "Download", true, busy, func() {
			setup.runLocal(s, w, id, "Downloading", nil)
		}))
	}
	other := "parakeet"
	if id == other {
		other = "orukeet"
	}
	if status.cached && (!status.ready || (id != "kokoro" && !inUse && setup.local[other].ready)) {
		actions = append(actions, action("delete-"+id, "Delete", false, busy, func() {
			setup.runLocal(s, w, id, "Deleting", nil)
		}))
	}
	if id != "kokoro" && inUse && status.ready && !busy {
		actions = append(actions, action("retry-"+id, "Reload", false, false, func() {
			s.prepareSpeech(false, true)
		}))
	}
	if setup.running == id && strings.HasPrefix(status.message, "Downloading") {
		actions = append(actions, action("stop-download", "Cancel", false, false, setup.cancel))
	}
	heading := []gui.View{bold(label, 15, ink), spacer(), text(note, 13, muted)}
	if inUse {
		heading = append(heading, text("Active", 13, accent))
	} else if id != "kokoro" && status.ready {
		heading = append(heading, action("provider-"+id, "Use", true, busy, func() {
			setup.useProvider(s, w, id, nil)
		}))
	}
	if s.speechLoading == id {
		note = s.speechStatus
	}
	content := []gui.View{speechRow(heading...)}
	if len(actions) > 0 {
		content = append(content, speechRow(actions...))
	}
	if s.speechLoading == id {
		content = append(content, wrapped(note, 13, muted))
	}
	if status.message != "" && status.message != note {
		content = append(content, wrapped(status.message, 13, muted))
	}
	return gui.Column(outlinedCard(card, gui.PadVH(8, 10), 4, content...))
}

func speechRow(content ...gui.View) gui.View {
	return gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, Spacing: gui.SpacingPx(8), VAlign: gui.VAlignMiddle, Content: content})
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

func (setup *speechSetup) mistralCard(s *state) gui.View {
	heading := []gui.View{bold("Mistral · Voxtral Small", 15, ink)}
	if config.Get[string](s.cfg, "speech_provider") == "mistral" {
		heading = append(heading, text("Active", 13, accent))
	}
	heading = append(heading, spacer(), bold("Set up", 14, accent))
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
	control := buttonConfig("provider-mistral", "Set up Mistral", 0, false, setup.savingProvider, open)
	control.Sizing, control.Height = gui.FillFit, 0
	control.Padding, control.SizeBorder = gui.PadVH(8, 10), gui.BorderPx(1)
	control.Colors.Border, control.Colors.BorderFocus = elevated, accent
	control.A11YCfg.A11YLabel = "Set up Mistral"
	status := "Cloud transcription · Requires an API key"
	if setup.hasKey {
		status = "API key saved in Windows Credential Manager"
	}
	control.Content = []gui.View{gui.Column(gui.ContainerCfg{
		Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, Spacing: gui.SpacingPx(4),
		Content: []gui.View{speechRow(heading...), wrapped(status, 13, muted)},
	})}
	return gui.Button(control)
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
			// Recheck a previously selected provider when the user supplies its missing key.
			if config.Get[string](s.cfg, "speech_provider") == "mistral" && !s.speechReady && s.speechLoading == "" {
				s.prepareSpeech(false, false)
			}
			// A closed window's save must not dismiss a later tutorial.
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
		toggleControl("show-mistral-key", "Show API key", setup.showKey, func(ctx gui.EventCtx) {
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
