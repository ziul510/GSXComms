// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

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
	slices.Sort(names)
	return names
}()

func assetDir(name string) string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(exe), name)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	// Allow source-tree runs to find assets relative to the working directory.
	return name
}

func (s *state) openAccents(onSaved func()) {
	if s.accentOpen {
		return
	}
	s.accentOpen = true
	ctx, cancel := context.WithCancel(s.ctx)
	var closeDialog func()
	closeDialog = s.dialog(accentWindow(), func() {
		// Cancel any installation still in progress when the accent window closes.
		cancel()
		s.accentOpen = false
	}, accentSetupView(ctx, s.accentsDir, filepath.Join(s.dir, "Accents"), s.settingsDraft(), func(enabled []string, fallback string, then func(error)) {
		s.save(accentPreferences(s.settingsDraft(), enabled, fallback), func(err error) {
			if err == nil && onSaved != nil {
				onSaved()
			}
			then(err)
		})
	}, func(*gui.Window) { closeDialog() }))
}

func accentWindow() gui.WindowCfg {
	return gui.WindowCfg{Title: "GSXComms Accent Setup", Width: 720, Height: 600, MinWidth: 520, MinHeight: 420}
}

func accentPreferences(prefs config.Values, enabled []string, fallback string) config.Values {
	prefs.Put("installed_packs", enabled)
	prefs.Put("default_accent", fallback)
	// Mark setup complete alongside the selection so startup can skip the wizard.
	prefs.Put("accent_setup_complete", true)
	return prefs
}

// Recover setup from complete recordings when the saved completion flag is missing
func restoreAccentSetup(recordings string, prefs config.Values) bool {
	enabled := config.Get[[]string](prefs, "installed_packs")
	var installed, options []string
	for _, pack := range packs {
		if (len(enabled) == 0 || slices.Contains(enabled, pack.name)) && legacyAccentPack(recordings, pack.name, pack.accents) {
			installed = append(installed, pack.name)
			options = append(options, pack.accents...)
		}
	}
	if len(installed) == 0 {
		return false
	}
	fallback := config.Get[string](prefs, "default_accent")
	if len(enabled) == 0 {
		enabled = installed
		if !slices.Contains(options, fallback) {
			fallback = slices.Min(options)
		}
	}
	accentPreferences(prefs, enabled, fallback)
	return true
}

func accentSetupView(ctx context.Context, archives, recordings string, prefs config.Values, save func([]string, string, func(error)), complete func(*gui.Window)) gui.ViewFunc {
	enabled := config.Get[[]string](prefs, "installed_packs")
	fallback := config.Get[string](prefs, "default_accent")
	available, selected := map[string]bool{}, map[string]bool{}
	for _, pack := range packs {
		extracted := legacyAccentPack(recordings, pack.name, pack.accents)
		if extracted {
			available[pack.name] = true
		} else if archive, err := zip.OpenReader(filepath.Join(archives, pack.name+".zip")); err == nil {
			available[pack.name] = true
			archive.Close()
		}
		selected[pack.name] = available[pack.name] && (slices.Contains(enabled, pack.name) ||
			(extracted && !config.Get[bool](prefs, "accent_setup_complete")))
	}
	var installed []string
	var saving bool
	var message string
	return func(w *gui.Window) gui.View {
		locked := saving || len(installed) > 0
		var body []gui.View
		var options []string
		for _, pack := range packs {
			if selected[pack.name] {
				options = append(options, pack.accents...)
			}
			details := strings.Join(pack.accents, ", ")
			if !available[pack.name] {
				details += " · Archive unavailable"
			}
			check := gui.ToggleCfg{
				ID: "pack-" + pack.name, Label: pack.name, Selected: selected[pack.name],
				Disabled: locked || !available[pack.name], Padding: gui.NoPadding, Size: gui.SomeF(20),
				TextStyleLabel: labelStyle(15, ink).Bold(), ColorSelect: accent,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					selected[pack.name] = !selected[pack.name]
					w.InvalidateLayout()
				},
			}
			if locked && check.Selected {
				// Keep the checkmark legible after the selection locks
				check.ColorSelect = accentHover
				check.TextStyle = gui.CurrentTheme().TextStyleDisabled.Bold()
				check.TextStyle.Color = ink
			}
			body = append(body, gui.Column(outlinedCard(card, gui.PadAll(12), 6, gui.Checkbox(check), wrapped(details, 13, muted))))
		}
		header := []gui.View{
			bold("Accent Setup", 24, ink),
			wrapped("Choose which regional voice packs to install. Recordings generated with ElevenLabs.", 14, muted),
			gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, Spacing: gui.SpacingPx(8), Content: []gui.View{
				button("select-all", "Select All", 0, false, locked || len(available) == 0, func() {
					for name := range available {
						selected[name] = true
					}
				}),
				button("deselect-all", "Deselect All", 0, false, locked, func() { clear(selected) }),
			}}),
		}
		if len(installed) > 0 {
			slices.Sort(options)
			defaultAccent := fallback
			if !slices.Contains(options, defaultAccent) {
				defaultAccent = options[0]
			}
			if message != "" {
				header = append(header, wrapped(message, 13, muted))
			}
			finish := buttonConfig("continue-accents", "Continue", 140, true, saving, func() {
				saving, message = true, "Saving selection…"
				// Both settings writers may finish off the accent window thread
				save(installed, defaultAccent, func(err error) {
					w.QueueCommand(func(w *gui.Window) {
						if w.CloseRequested() || ctx.Err() != nil {
							return
						}
						if err == nil {
							complete(w)
							return
						}
						saving, message = false, err.Error()
						w.InvalidateLayout()
					})
				})
			})
			finish.Colors = gui.ColorSet{Base: green, Hover: gui.Hex(0x46df82)}
			return page("accent-packs", header, body, []gui.View{
				text("Default accent", 13, ink),
				formSelect(gui.SelectCfg{
					ID: "wizard-default-accent", Items: options, Selected: []string{defaultAccent},
					A11YCfg: gui.A11YCfg{A11YLabel: "Default accent"}, MinWidth: 150, MaxWidth: 150, Disabled: saving,
					OnSelect: func(values []string, ctx gui.EventCtx) {
						if len(values) > 0 {
							fallback = values[0]
						}
						ctx.Consume()
						w.InvalidateLayout()
					},
				}),
				spacer(), gui.Button(finish),
			})
		}
		return page("accent-packs", header, body, []gui.View{
			wrapped(message, 13, muted), spacer(), button("save-accents", "Save", 100, true, saving || len(options) == 0, func() {
				enabled := []string{}
				for _, pack := range packs {
					if selected[pack.name] {
						enabled = append(enabled, pack.name)
					}
				}
				saving, message = true, "Installing selected packs…"
				go func() {
					err := installAccentPacks(ctx, archives, recordings, enabled)
					w.QueueCommand(func(w *gui.Window) {
						if w.CloseRequested() || ctx.Err() != nil {
							return
						}
						saving, message = false, ""
						if err != nil {
							message = err.Error()
						} else {
							installed = enabled
						}
						w.InvalidateLayout()
					})
				}()
			}),
		})
	}
}

func (s *state) accentCue(dir, pack, accentName, service, option, voice string) ([]byte, int) {
	folder, action := service, ""
	switch service {
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
	clip := strings.ToLower(filepath.Join(accentName, folder, voice+action+"11.ai.mp3"))
	// Try the current pack layout before looking in older extraction locations.
	for _, relative := range []string{pack, "Extracted", ""} {
		if pcm, rate := s.loadClip(filepath.Join(dir, relative, clip)); len(pcm) > 0 {
			return pcm, rate
		}
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
	// Cap decoded audio separately because a small MP3 can expand into a large buffer.
	pcm, err := io.ReadAll(io.LimitReader(decoder, (8<<20)+1))
	if err != nil || len(pcm) > 8<<20 || len(pcm)%4 != 0 {
		return nil, 0
	}
	return pcm, decoder.SampleRate()
}

type cachedClip struct {
	path     string
	size     int64
	modified time.Time
	pcm      []byte
	rate     int
}

// Cache decoded recordings only on the playback lane
func (s *state) loadClip(path string) ([]byte, int) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0
	}
	for i, clip := range s.clipCache {
		if clip.path == path {
			s.clipCache = slices.Delete(s.clipCache, i, i+1)
			// Decode the file again if its size or modification time changed.
			if clip.size == info.Size() && clip.modified.Equal(info.ModTime()) {
				// Move cache hits to the end so eviction drops the least recently used clip.
				s.clipCache = append(s.clipCache, clip)
				return clip.pcm, clip.rate
			}
			break
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer file.Close()
	pcm, rate := decodeAccentClip(file)
	if len(pcm) > 0 {
		if len(s.clipCache) == 32 {
			s.clipCache = slices.Delete(s.clipCache, 0, 1)
		}
		s.clipCache = append(s.clipCache, cachedClip{path, info.Size(), info.ModTime(), pcm, rate})
	}
	return pcm, rate
}

func installedCrewAccent(enabled []string, candidate string) (packName, accentName string) {
	for _, pack := range packs {
		if !slices.Contains(enabled, pack.name) {
			continue
		}
		for _, name := range pack.accents {
			// Match overrides without case sensitivity and return the pack's spelling for filenames.
			if strings.EqualFold(name, candidate) {
				return pack.name, name
			}
		}
	}
	return "", ""
}

// Share the candidate order between playback and the debug location summary
func crewAccentCandidates(prefs config.Values, location *geo.Position, overrides map[string]string) []string {
	var pos geo.Position
	if location != nil {
		pos = *location
	}
	var names []string
	if forced := config.Get[string](prefs, "force_accent"); forced != "Disabled" {
		names = append(names, forced)
	}
	names = append(names, geo.AccentCandidates(pos, overrides)...)
	names = append(names, config.Get[string](prefs, "default_accent"))
	return append(names, accents...)
}

func (s *state) regionalCue(next playback) ([]byte, int, string) {
	enabled := config.Get[[]string](next.prefs, "installed_packs")
	if len(enabled) == 0 {
		return nil, 0, ""
	}
	recordings := filepath.Join(s.dir, "Accents")
	for _, candidate := range crewAccentCandidates(next.prefs, next.location, geo.Overrides(s.dir)) {
		pack, name := installedCrewAccent(enabled, candidate)
		if name == "" {
			continue
		}
		// Try the next accent if this pack lacks the recording for the requested cue.
		if pcm, rate := s.accentCue(recordings, pack, name, next.cue, next.option, next.voice); len(pcm) > 0 {
			return pcm, rate, name
		}
	}
	return nil, 0, ""
}
