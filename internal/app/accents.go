// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"archive/zip"
	"bytes"
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
	dir := assetDir("Accents")
	prefs := s.settingsDraft()
	enabled := config.Get[[]string](prefs, "installed_packs")
	available, selected := map[string]bool{}, map[string]bool{}
	for _, pack := range packs {
		archive, err := zip.OpenReader(filepath.Join(dir, pack.name+".zip"))
		if err == nil {
			available[pack.name] = true
			archive.Close()
		}
		selected[pack.name] = slices.Contains(enabled, pack.name)
	}
	var saving bool
	var message string
	s.dialog(gui.WindowCfg{Title: "GSXComms Accent Setup", Width: 720, Height: 600, MinWidth: 520, MinHeight: 420}, func() { s.accentOpen = false }, func(w *gui.Window) gui.View {
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
			wrapped("Choose regional crew voices. Accent recordings generated with ElevenLabs.", 14, muted),
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
				next := s.settingsDraft()
				next.Put("installed_packs", enabled)
				next.Put("accent_setup_complete", true)
				saving, message = true, "Saving…"
				s.save(next, func(err error) {
					if err != nil {
						saving, message = false, err.Error()
						w.InvalidateLayout()
						return
					}
					w.Close()
					s.accentOpen = false
				})
			}),
		})
	})
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
	member := accentName + "/" + folder + "/" + voice + action + "11.ai.mp3"
	for _, pack := range packs {
		if !slices.Contains(enabled, pack.name) || !slices.ContainsFunc(pack.accents, func(name string) bool {
			return strings.EqualFold(name, accentName)
		}) {
			continue
		}
		z, err := zip.OpenReader(filepath.Join(dir, pack.name+".zip"))
		if err != nil {
			return nil, 0
		}
		defer z.Close()
		for _, file := range z.File {
			if strings.EqualFold(file.Name, member) {
				return decodeAccentClip(file)
			}
		}
	}
	return nil, 0
}

func decodeAccentClip(file *zip.File) ([]byte, int) {
	if file.UncompressedSize64 > 1<<20 {
		return nil, 0
	}
	source, err := file.Open()
	if err != nil {
		return nil, 0
	}
	defer source.Close()
	raw, err := io.ReadAll(source)
	if err != nil {
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
	pcm, rate := accentCue(s.accentsDir, enabled, name, next.cue, next.option, s.crewVoice)
	if len(pcm) == 0 && !strings.EqualFold(name, fallback) {
		return accentCue(s.accentsDir, enabled, fallback, next.cue, next.option, s.crewVoice)
	}
	return pcm, rate
}
