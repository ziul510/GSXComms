// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/hajimehoshi/go-mp3"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/geo"
)

var packs = []struct {
	name    string
	accents []string
}{
	{"English-Speaking Countries", []string{"American", "British", "Irish", "Scottish", "Australian"}},
	{"Latin & Southern Europe", []string{"Spanish", "Portuguese", "French", "Italian", "Romanian"}},
	{"Western & Northern Europe", []string{"German", "Swedish", "Finnish"}},
	{"Eastern Europe", []string{"Croatian", "Russian"}},
	{"Middle East & North Africa", []string{"Arabic", "Turkish", "Greek"}},
	{"Sub-Saharan Africa", []string{"Nigerian"}},
	{"South & Southeast Asia", []string{"Indian", "Malay", "Filipino", "Singaporean"}},
	{"East Asia & Pacific", []string{"Chinese", "Japanese", "Korean"}},
}

var accents = func() []string {
	var names []string
	for _, pack := range packs {
		names = append(names, pack.accents...)
	}
	return names
}()

func assetDir(name string) string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(exe), name)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return name
}

func (s *state) openAccents() {
	if s.accentOpen {
		return
	}
	s.accentOpen = true
	ctx, cancel := context.WithCancel(s.ctx)
	var closeDialog func()
	closeDialog = s.dialog(accentWindow(), func() {
		cancel()
		s.accentOpen = false
	}, accentSetupView(ctx, s.accentsDir, filepath.Join(s.dir, "Accents"), s.settingsDraft(), func(enabled []string, then func(error)) {
		s.save(accentPreferences(s.settingsDraft(), enabled), then)
	}, func(*gui.Window) { closeDialog() }))
}

func accentWindow() gui.WindowCfg {
	return gui.WindowCfg{Title: "GSXComms Accent Setup", Width: 720, Height: 600, MinWidth: 520, MinHeight: 420}
}

func accentPreferences(prefs config.Values, enabled []string) config.Values {
	prefs.Put("installed_packs", enabled)
	prefs.Put("accent_setup_complete", true)
	return prefs
}

func accentSetupView(ctx context.Context, archives, recordings string, prefs config.Values, save func([]string, func(error)), complete func(*gui.Window)) gui.ViewFunc {
	enabled := config.Get[[]string](prefs, "installed_packs")
	available, selected := map[string]bool{}, map[string]bool{}
	for _, pack := range packs {
		if accentPackInstalled(recordings, pack.name) {
			available[pack.name] = true
		} else if archive, err := zip.OpenReader(filepath.Join(archives, pack.name+".zip")); err == nil {
			available[pack.name] = true
			archive.Close()
		}
		selected[pack.name] = available[pack.name] && slices.Contains(enabled, pack.name)
	}
	var saving bool
	var message string
	return func(w *gui.Window) gui.View {
		var body []gui.View
		for _, pack := range packs {
			details := strings.Join(pack.accents, ", ")
			if !available[pack.name] {
				details += " · Archive unavailable"
			}
			check := gui.Checkbox(gui.ToggleCfg{
				ID: "pack-" + pack.name, Label: pack.name, Selected: selected[pack.name],
				Disabled: saving || !available[pack.name], Padding: gui.NoPadding, Size: gui.SomeF(20),
				TextStyleLabel: labelStyle(15, ink).Bold(), ColorSelect: accent,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					selected[pack.name] = !selected[pack.name]
					w.InvalidateLayout()
				},
			})
			body = append(body, gui.Column(outlinedCard(card, gui.PadAll(12), 6, check, wrapped(details, 13, muted))))
		}
		header := []gui.View{
			bold("Accent Setup", 24, ink),
			wrapped("Choose which regional voice packs to install. Recordings generated with ElevenLabs.", 14, muted),
			gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, Spacing: gui.SpacingPx(8), Content: []gui.View{
				button("select-all", "Select All", 0, false, saving || len(available) == 0, func() {
					for name := range available {
						selected[name] = true
					}
				}),
				button("deselect-all", "Deselect All", 0, false, saving, func() { clear(selected) }),
			}}),
		}
		return page("accent-packs", header, body, []gui.View{
			wrapped(message, 13, muted), spacer(), button("save-accents", "Save", 100, true, saving, func() {
				enabled := []string{}
				for _, pack := range packs {
					if selected[pack.name] {
						enabled = append(enabled, pack.name)
					}
				}
				saving, message = true, "Installing selected packs…"
				done := func(err error) {
					if err == nil {
						complete(w)
					} else {
						saving, message = false, err.Error()
						w.InvalidateLayout()
					}
				}
				go func() {
					err := installAccentPacks(ctx, archives, recordings, enabled)
					w.QueueCommand(func(w *gui.Window) {
						if w.CloseRequested() || ctx.Err() != nil {
							return
						}
						if err != nil {
							done(err)
							return
						}
						saving, message = true, "Saving selection…"
						w.InvalidateLayout()
						save(enabled, done)
					})
				}()
			}),
		})
	}
}

func accentCue(dir string, enabled []string, accentName, service, option, voice string) ([]byte, int) {
	folder, action := service, ""
	switch service {
	case "connected":
		return nil, 0
	case "greeting":
		folder = "goahead"
	case "error":
		folder = "recogerror"
	case "jetway", "stairs", "gpu":
		if service == "jetway" {
			folder = "jetways"
		}
		action = "connect"
		option = strings.ToLower(option)
		for _, word := range []string{"disconnect", "remove", "retract", "undock"} {
			if strings.Contains(option, word) {
				action = "disconnect"
				break
			}
		}
	}
	for _, pack := range packs {
		if !slices.Contains(enabled, pack.name) || !slices.ContainsFunc(pack.accents, func(name string) bool {
			return strings.EqualFold(name, accentName)
		}) {
			continue
		}
		clip := strings.ToLower(filepath.Join(accentName, folder, voice+action+"11.ai.mp3"))
		file, err := os.Open(filepath.Join(dir, pack.name, clip))
		if err != nil {
			return nil, 0
		}
		defer file.Close()
		return decodeAccentClip(file)
	}
	return nil, 0
}

const maxAccentClipBytes = 1 << 20

func decodeAccentClip(source io.Reader) ([]byte, int) {
	raw, err := io.ReadAll(io.LimitReader(source, maxAccentClipBytes+1))
	if err != nil || len(raw) > maxAccentClipBytes {
		return nil, 0
	}
	decoder, err := mp3.NewDecoder(bytes.NewReader(raw))
	if err != nil {
		return nil, 0
	}
	pcm, err := io.ReadAll(io.LimitReader(decoder, (8<<20)+1))
	if err != nil || len(pcm) > 8<<20 || len(pcm)%4 != 0 {
		return nil, 0
	}
	// The decoder produces stereo PCM16 and the playback path expects mono
	for i := 0; i < len(pcm); i += 4 {
		left := int32(int16(binary.LittleEndian.Uint16(pcm[i:])))
		right := int32(int16(binary.LittleEndian.Uint16(pcm[i+2:])))
		binary.LittleEndian.PutUint16(pcm[i/2:], uint16((left+right)/2))
	}
	return pcm[:len(pcm)/2], decoder.SampleRate()
}

func (s *state) regionalCue(next playback) ([]byte, int) {
	enabled := config.Get[[]string](next.prefs, "installed_packs")
	if len(enabled) == 0 || next.cue == "connected" {
		return nil, 0
	}
	var pos geo.Position
	if next.location != nil {
		pos = *next.location
	}
	fallback := config.Get[string](next.prefs, "default_accent")
	forced := config.Get[string](next.prefs, "force_accent")
	name := geo.Accent(pos, geo.Overrides(s.dir), fallback, forced)
	recordings := filepath.Join(s.dir, "Accents")
	pcm, rate := accentCue(recordings, enabled, name, next.cue, next.option, s.crewVoice)
	if len(pcm) == 0 && !strings.EqualFold(name, fallback) {
		return accentCue(recordings, enabled, fallback, next.cue, next.option, s.crewVoice)
	}
	return pcm, rate
}
