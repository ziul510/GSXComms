// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
)

func labelStyle(size float32, color gui.Color) gui.TextStyle {
	return gui.TextStyle{Size: size, Color: color, Family: "Segoe UI"}
}

func text(label string, size float32, color gui.Color) gui.View {
	return gui.Label(label, labelStyle(size, color))
}

func bold(label string, size float32, color gui.Color) gui.View {
	return gui.Label(label, labelStyle(size, color).Bold())
}

func wrapped(label string, size float32, color gui.Color) gui.View {
	return gui.Text(gui.TextCfg{Text: label, Mode: gui.TextModeWrap, TextStyle: labelStyle(size, color)})
}

func buttonConfig(id, label string, width float32, primary, disabled bool, onClick func()) gui.ButtonCfg {
	style := labelStyle(12, ink)
	colors := gui.ColorSet{Base: card, Hover: elevated}
	if primary {
		style = labelStyle(13, bg).Bold()
		colors = gui.ColorSet{Base: accent, Hover: accentHover}
	}
	sizing := gui.FitFixed
	if width > 0 {
		sizing = gui.FixedFixed
	}
	return gui.ButtonCfg{
		ID: id, Width: width, Height: 32, Sizing: sizing, Radius: gui.RadiusPx(6), Padding: gui.PadVH(0, 8),
		SizeBorder: gui.NoBorder, Colors: colors, Disabled: disabled, VAlign: gui.Some(gui.VAlignMiddle),
		Content: []gui.View{gui.Label(label, style)},
		OnClick: func(ctx gui.EventCtx) { ctx.Consume(); onClick(); ctx.Window.InvalidateLayout() },
	}
}

func button(id, label string, width float32, primary, disabled bool, onClick func()) gui.View {
	return gui.Button(buttonConfig(id, label, width, primary, disabled, onClick))
}

func spacer() gui.View {
	return gui.Rectangle(gui.RectangleCfg{Sizing: gui.FillFixed, Height: 1})
}

func gap(width, height float32) gui.View {
	return gui.Rectangle(gui.RectangleCfg{Sizing: gui.FixedFixed, Width: width, Height: height})
}

var slimScrollbar = gui.ScrollbarCfg{
	Size: 8, RadiusThumb: 4, GapEdge: gui.SomeF(4), GapEnd: gui.SomeF(4),
	ColorThumb: gui.Hex(0x6e6e6e), ColorBackground: gui.ColorTransparent,
}

func mainView(w *gui.Window) gui.View {
	s := gui.State[state](w)
	status, statusColor, gsxColor := "Offline", muted, muted
	switch {
	case s.busy:
		status, statusColor, gsxColor = "Syncing", orange, orange
	case s.connected:
		status, statusColor, gsxColor = "Online", green, red
		if s.ready {
			gsxColor = green
		}
	}
	speed, speedColor := "-- kts", muted
	if s.connected && s.speedKnown {
		speed, speedColor = fmt.Sprintf("%d kts", int(s.speed)), accent
		if s.speed > 50 {
			speedColor = orange
		}
	}
	bar := []gui.View{bold("● "+status, 12, statusColor), bold("● GSX", 12, gsxColor), spacer()}
	if config.Get[bool](s.cfg, "show_topmost") {
		active := config.Get[bool](s.cfg, "topmost_active")
		topmost := buttonConfig("top-most", "Top Most", 86, active, false, func() {
			next := s.settingsDraft()
			next.Put("topmost_active", !config.Get[bool](next, "topmost_active"))
			s.save(next, nil)
		})
		topmost.Height, topmost.SizeBorder = 28, gui.BorderPx(1)
		topmost.Colors.Border = accent
		if !active {
			topmost.Content = []gui.View{text("Top Most", 11, muted)}
			topmost.Colors = gui.ColorSet{Base: elevated, Hover: card, Border: bg, BorderFocus: accent}
		}
		bar = append(bar, gui.Button(topmost), gap(4, 1))
	}
	bar = append(bar, bold(speed, 12, speedColor))
	pttColor := ink
	if s.ptt != "Idle" {
		pttColor = orange
	}
	binding := config.Get[config.Binding](s.cfg, "ptt_bind").Description
	body := []gui.View{gui.Row(gui.ContainerCfg{
		Sizing: gui.FillFixed, Height: 48, Color: card, ColorBorder: elevated, Radius: gui.RadiusPx(6),
		Padding: gui.PadVH(0, 11), SizeBorder: gui.BorderPx(1), VAlign: gui.VAlignMiddle, Spacing: gui.SpacingPx(6),
		Content: []gui.View{text("PTT", 11, muted), bold(s.ptt, 13, pttColor), spacer(), text(binding, 11, muted)},
	})}
	if config.Get[bool](s.cfg, "enable_keyboard_input") {
		const label = "Type command: (e.g. 'connect jetway')"
		body = append(body, gui.Column(gui.ContainerCfg{
			Sizing: gui.FillFixed, Height: 76, Color: card, ColorBorder: elevated, Radius: gui.RadiusPx(6),
			Padding: gui.PadVH(7, 11), SizeBorder: gui.BorderPx(1), Spacing: gui.SpacingPx(4),
			Content: []gui.View{
				gui.Row(gui.ContainerCfg{Sizing: gui.FillFixed, Height: 28, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, VAlign: gui.VAlignMiddle, Content: []gui.View{text(label, 11, muted)}}),
				gui.Input(gui.InputCfg{
					ID: "command", Sizing: gui.FillFixed, Height: 28, Text: s.input, TextStyle: labelStyle(12, ink),
					Radius: gui.RadiusPx(5), SizeBorder: gui.BorderPx(2), Padding: gui.PadVH(0, 6),
					Colors:        gui.ColorSet{Base: elevated, Border: bg, BorderFocus: accent},
					A11YCfg:       gui.A11YCfg{A11YLabel: label},
					OnTextChanged: func(value string, _ gui.EventCtx) { s.input = value },
					OnEnter: func(ctx gui.EventCtx) {
						ctx.Consume()
						value := s.input
						s.input = ""
						s.submit(value)
						ctx.Window.InvalidateLayout()
					},
				}),
			},
		}))
	}
	logArea := gui.ContainerCfg{
		ID: "conversation", Sizing: gui.FillFill, Scrollable: true, ScrollbarCfgY: &slimScrollbar,
		Color: terminal, ColorBorder: elevated, Radius: gui.RadiusPx(6), Padding: gui.PadAll(13),
		Spacing: gui.SpacingPx(3), SizeBorder: gui.BorderPx(1),
	}
	if s.overlay {
		logArea.ColorBorder, logArea.Padding = accent, gui.PadAll(8)
		title := s.menu.Title
		if title == "" {
			title = "GSX Menu"
		}
		logArea.Content = append(logArea.Content, bold(title, 11, ink))
		if len(s.menu.Options) == 0 {
			logArea.Content = append(logArea.Content, text("No GSX menu available", 11, muted))
		}
		for i, item := range s.menu.Options {
			label := fmt.Sprintf("%d. %s", i+1, item)
			option := buttonConfig("option-"+strconv.Itoa(i), label, 0, i == s.highlight, false, func() { s.choose(i) })
			fg := ink
			if i == s.highlight {
				fg = bg
			}
			option.Sizing, option.Height, option.Radius = gui.FillFit, 0, gui.RadiusPx(4)
			option.Padding, option.HAlign = gui.PadVH(7, 16), gui.Some(gui.HAlignLeft)
			option.Content = []gui.View{wrapped(label, 11, fg)}
			logArea.Content = append(logArea.Content, gui.Button(option))
		}
	} else {
		logArea.Content = []gui.View{gui.Text(gui.TextCfg{ID: "log-content", Text: strings.Join(s.lines, "\n"), Mode: gui.TextModeWrap, TextStyle: gui.TextStyle{Size: 12, Family: "Consolas", Color: terminalInk, LetterSpacing: 0.4}, Focusable: true})}
	}
	body = append(body, gui.Column(logArea))
	connect := buttonConfig("connect", "Connect", 120, true, s.busy, s.toggleConnect)
	switch {
	case s.busy:
		connect.Content = []gui.View{bold("Connecting…", 13, bg)}
		connect.Colors = gui.ColorSet{Base: orange, Disabled: orange}
	case s.connected:
		connect.Content = []gui.View{bold("Disconnect", 13, bg)}
		connect.Colors = gui.ColorSet{Base: green, Hover: gui.Hex(0x48df85)}
	}
	connect.Height = 36
	menuLabel := "Show GSX Menu"
	if s.overlay {
		menuLabel = "Hide GSX Menu"
	}
	menu := buttonConfig("gsx-menu", menuLabel, 120, false, !s.ready, s.toggleMenu)
	settings := buttonConfig("settings", "Settings", 80, false, false, s.openSettings)
	for _, control := range []*gui.ButtonCfg{&menu, &settings} {
		control.Height = 36
		control.Colors = gui.ColorSet{Base: gui.ColorTransparent, Hover: elevated, Disabled: gui.ColorTransparent}
	}
	menu.Content = []gui.View{text(menuLabel, 12, muted)}
	settings.Content = []gui.View{text("Settings", 12, muted)}
	return gui.Column(gui.ContainerCfg{
		Sizing: gui.FillFill, Color: bg, Radius: gui.NoRadius, Padding: gui.NoPadding, Spacing: gui.NoSpacing, SizeBorder: gui.NoBorder,
		Content: []gui.View{
			gui.Row(gui.ContainerCfg{Sizing: gui.FillFixed, Height: 48, Color: card, Radius: gui.NoRadius, Padding: gui.PadVH(0, 12), SizeBorder: gui.NoBorder, VAlign: gui.VAlignMiddle, Spacing: gui.SpacingPx(10), Content: bar}),
			gui.Row(gui.ContainerCfg{Sizing: gui.FillFixed, Height: 52, Padding: gui.PadVH(0, 12), SizeBorder: gui.NoBorder, VAlign: gui.VAlignMiddle, Content: []gui.View{bold("GSXComms", 22, ink)}}),
			gui.Column(gui.ContainerCfg{Sizing: gui.FillFill, Padding: gui.PadVH(0, 12), Spacing: gui.SpacingPx(8), SizeBorder: gui.NoBorder, Content: body}),
			gui.Row(gui.ContainerCfg{
				Sizing: gui.FillFixed, Height: 56, Padding: gui.PadVH(0, 12), VAlign: gui.VAlignMiddle, SizeBorder: gui.NoBorder,
				Spacing: gui.SpacingPx(8), Content: []gui.View{gui.Button(connect), spacer(), gui.Button(menu), gui.Button(settings)},
			}),
		},
	})
}

func (s *state) dialog(cfg gui.WindowCfg, onClose func(), view gui.ViewFunc) func() {
	var window *gui.Window
	closed := false
	closeDialog := func() {
		if !closed {
			closed = true
			onClose()
		}
		if window != nil {
			window.Close()
		}
	}
	cfg.IconPNG = s.icon
	cfg.OnInit = func(w *gui.Window) {
		window = w
		if closed {
			w.Close()
			return
		}
		w.TitlebarDark(true)
		w.SetView(view)
	}
	cfg.OnCloseRequest = func(*gui.Window) { closeDialog() }
	s.host.OpenWindow(cfg)
	return closeDialog
}

func page(id string, header, body, footer []gui.View) gui.View {
	scrollPadding := gui.NoPadding
	scrollPadding.Right = 16
	return gui.Column(gui.ContainerCfg{Sizing: gui.FillFill, Color: bg, Padding: gui.PadAll(20), Spacing: gui.SpacingPx(12), SizeBorder: gui.NoBorder, Content: []gui.View{
		gui.Column(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, Spacing: gui.SpacingPx(4), SizeBorder: gui.NoBorder, Content: header}),
		gui.Column(gui.ContainerCfg{ID: id, Sizing: gui.FillFill, Scrollable: true, ScrollbarCfgY: &slimScrollbar, ScrollMode: gui.ScrollVerticalOnly, Padding: scrollPadding, Spacing: gui.SpacingPx(8), SizeBorder: gui.NoBorder, Content: body}),
		gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, VAlign: gui.VAlignMiddle, Spacing: gui.SpacingPx(8), SizeBorder: gui.NoBorder, Content: footer}),
	}})
}

func outlinedCard(fill gui.Color, padding gui.Padding, spacing float32, content ...gui.View) gui.ContainerCfg {
	return gui.ContainerCfg{
		Sizing: gui.FillFit, Color: fill, ColorBorder: elevated, SizeBorder: gui.BorderPx(1),
		Radius: gui.RadiusPx(6), Padding: padding, Spacing: gui.SpacingPx(spacing), Content: content,
	}
}

func labelRow(label gui.View, height float32) gui.View {
	return gui.Row(gui.ContainerCfg{
		Sizing: gui.FillFixed, Height: height, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, VAlign: gui.VAlignMiddle,
		Content: []gui.View{label},
	})
}

func settingsField(label string, control gui.View) gui.View {
	return gui.Column(gui.ContainerCfg{
		Sizing: gui.FillFit, Padding: gui.NoPadding, Spacing: gui.SpacingPx(6), SizeBorder: gui.NoBorder,
		Content: []gui.View{labelRow(text(label, 11, muted), 28), control},
	})
}

func formInput(cfg gui.InputCfg) gui.View {
	cfg.Sizing, cfg.TextStyle = gui.FillFit, labelStyle(12, ink)
	cfg.Padding, cfg.Radius, cfg.SizeBorder = gui.PadVH(6, 7), gui.RadiusPx(5), gui.BorderPx(1)
	cfg.Colors = gui.ColorSet{Base: elevated, Border: bg, BorderFocus: accent}
	return gui.Input(cfg)
}

func formSelect(cfg gui.SelectCfg) gui.View {
	cfg.Sizing, cfg.TextStyle = gui.FillFit, labelStyle(11, ink)
	cfg.Padding, cfg.Radius, cfg.SizeBorder = gui.PadVH(7, 7), gui.RadiusPx(5), gui.NoBorder
	cfg.Colors = gui.ColorSet{Base: card, Hover: elevated, BorderFocus: accent}
	return gui.Select(cfg)
}

func toggleControl(id, label string, selected bool, onClick func(gui.EventCtx)) gui.View {
	track := disabledInk
	if selected {
		track = accent
	}
	return gui.Switch(gui.SwitchCfg{
		ID: id, Label: label, Selected: selected, TextStyleLabel: labelStyle(11, ink),
		Width: gui.SomeF(34), Height: gui.SomeF(18), SizeBorder: gui.BorderPx(1), Padding: gui.NoPadding,
		ColorSelect: ink, ColorUnselect: ink, Colors: gui.ColorSet{Base: track, Border: track, BorderFocus: ink},
		OnClick: onClick,
	})
}

func (s *state) settingsDraft() config.Values {
	if s.pendingSave != nil {
		return s.pendingSave.values.Copy()
	}
	return s.cfg.Copy()
}

func (s *state) mergeSettings(base, draft config.Values) config.Values {
	next := s.settingsDraft()
	// Keep newer saves for fields this dialog didn't edit.
	for name, value := range draft {
		if !bytes.Equal(base[name], value) {
			next[name] = bytes.Clone(value)
		}
	}
	return next
}

func (s *state) save(next config.Values, then func(error)) {
	req := &saveRequest{next, then}
	select {
	case s.saves <- req:
		s.pendingSave = req
	default:
		err := errors.New("settings save queue is busy")
		if then != nil {
			then(err)
		} else {
			s.log("ERR", err.Error())
		}
	}
}

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

func (s *state) openSettings() {
	if s.settingsOpen {
		return
	}
	s.settingsOpen = true
	base := s.settingsDraft()
	draft := base.Copy()
	var message string
	var saving, closed bool
	var closePresets func()
	closeSettings := func() {
		if !closed {
			closed = true
			if s.binding != nil {
				s.binding(config.Binding{})
			}
			if closePresets != nil {
				closePresets()
			}
			s.settingsOpen = false
		}
	}
	inputs, outputs := []string{"Default"}, []string{"Default"}
	view := func(w *gui.Window) gui.View {
		var body []gui.View
		section := func(name, description string) {
			body = append(body, gui.Column(outlinedCard(card, gui.PadVH(12, 11), 4,
				labelRow(bold(name, 13, ink), 26), labelRow(wrapped(description, 11, muted), 26))))
		}
		toggle := func(key, label string) {
			body = append(body, labelRow(toggleControl(key, label, config.Get[bool](draft, key), func(ctx gui.EventCtx) {
				draft.Put(key, !config.Get[bool](draft, key))
				ctx.Consume()
				ctx.Window.InvalidateLayout()
			}), 24))
		}
		pick := func(key, label string, items []string) {
			value := config.Get[string](draft, key)
			if !slices.Contains(items, value) && value != "" {
				items = append(slices.Clone(items), value)
			}
			body = append(body, settingsField(label, formSelect(gui.SelectCfg{
				ID: key, A11YCfg: gui.A11YCfg{A11YLabel: label}, Items: items, Selected: []string{value},
				OnSelect: func(values []string, ctx gui.EventCtx) {
					if len(values) > 0 {
						draft.Put(key, values[0])
					}
					ctx.Consume()
				}})))
		}
		slider := func(key, label string, minv, maxv, step float32) {
			control := gui.Slider(gui.SliderCfg{
				ID: key, A11YCfg: gui.A11YCfg{A11YLabel: label}, Min: minv, Max: maxv, Step: step,
				Value: float32(config.Get[float64](draft, key)), Sizing: gui.FillFixed, Height: 16,
				Size: 4, ThumbSize: 16, SizeBorder: gui.NoBorder, Radius: gui.NoRadius,
				Padding: gui.NoPadding, Color: bg, ColorLeft: accent, ColorThumb: ink,
				OnChange: func(v float32, ctx gui.EventCtx) {
					if step == 1 {
						draft.Put(key, int(v))
					} else {
						draft.Put(key, v)
					}
					ctx.Consume()
				},
			})
			body = append(body, settingsField(label, gui.Row(gui.ContainerCfg{
				Sizing: gui.FillFixed, Height: 16, Padding: gui.PadVH(0, 6), SizeBorder: gui.NoBorder,
				Content: []gui.View{control},
			})))
		}
		action := func(id, label string, disabled bool, onClick func()) {
			control := buttonConfig(id, label, 0, false, disabled, onClick)
			control.Sizing = gui.FillFixed
			body = append(body, gui.Button(control))
		}
		section("Audio Devices", "Choose preferred input and output paths")
		pick("input_device_name", "Microphone", inputs)
		pick("output_device_name", "Speaker", outputs)
		section("Audio Mix", "Tune static and crew voice levels")
		toggle("enable_static", "Enable radio static")
		slider("volume_static", "Static volume", 0, 0.5, 0.01)
		slider("volume_voice", "Voice volume", 0, 1, 0.01)
		slider("max_recording_seconds", "Max recording (seconds)", 10, 120, 1)
		section("Speech Recognition", "Pick a local model or the cloud Voxtral Small model")
		provider := config.Get[string](draft, "speech_provider")
		var modelRows []gui.View
		for _, model := range []struct{ id, label string }{
			{"parakeet", "Standard, Recommended (Parakeet v2 0.6b)"},
			{"orukeet", "Enhanced (Orukeet)"},
			{"mistral", "Voxtral Small (cloud)"},
		} {
			right := []gui.View{button("provider-"+model.id, "Use", 56, true, false, func() { draft.Put("speech_provider", model.id) })}
			if provider == model.id {
				right = []gui.View{text("Active", 11, accent)}
			}
			row := outlinedCard(card, gui.PadVH(5, 11), 8, bold(model.label, 12, ink), spacer())
			row.Sizing, row.Height, row.VAlign = gui.FillFixed, 44, gui.VAlignMiddle
			row.Content = append(row.Content, right...)
			modelRows = append(modelRows, gui.Row(row))
		}
		body = append(body, gui.Column(gui.ContainerCfg{
			Sizing: gui.FillFit, Padding: gui.NoPadding, Spacing: gui.SpacingPx(10), SizeBorder: gui.NoBorder, Content: modelRows,
		}), wrapped("Parakeet and Orukeet run locally. Voxtral Small sends recordings to Mistral.", 11, muted))
		action("speech-models", "Speech model setup", false, func() { s.openSpeech(w) })
		section("Accent", "Default accent and regional packs")
		pick("default_accent", "Default accent", accents)
		action("accent-setup", "Open Accent Setup", false, s.openAccents)
		section("Presets", "Bundle multiple services behind a codeword and pick the run order")
		presets := config.Get[[]config.Preset](draft, "presets")
		summary := "No presets yet. Open Preset Manager to create one."
		if len(presets) == 1 {
			summary = "1 preset"
		} else if len(presets) > 1 {
			summary = fmt.Sprintf("%d presets", len(presets))
		}
		body = append(body, text(summary, 11, muted))
		action("presets", "Open Preset Manager", closePresets != nil || saving, func() {
			closePresets = s.openPresets(draft, w, func() { closePresets = nil; w.InvalidateLayout() })
		})
		section("Behavior", "The behavior of the app")
		toggle("enable_menu_matching", "Broader mode")
		toggle("enable_gsx_menu_text_correction", "Match STT to GSX menu options")
		toggle("read_gsx_menus_aloud_after_keyword", "Read GSX menus aloud after a keyword")
		if config.Get[bool](draft, "read_gsx_menus_aloud_after_keyword") {
			body = append(body, formInput(gui.InputCfg{ID: "menu-reader-keyword", Label: "Keyword:", Placeholder: "e.g. read menu", Text: config.Get[string](draft, "gsx_menu_reader_keyword"), OnTextChanged: func(v string, _ gui.EventCtx) { draft.Put("gsx_menu_reader_keyword", v) }}))
		}
		toggle("auto_connect_sim", "Auto-connect to simulator")
		toggle("auto_select_operator", "Auto-select operator")
		toggle("mute_gsx_audio_during_crew_voice", "Mute Couatl/GSX during speech")
		section("Controls", "PTT binding, live input handling and typing")
		toggle("enable_keyboard_input", "Enable keyboard typing")
		toggle("minimize_to_tray", "Minimize to tray")
		toggle("start_minimized", "Start minimized")
		toggle("show_topmost", "Show Top Most")
		body = append(body, settingsField("Current PTT binding", bold(config.Get[config.Binding](draft, "ptt_bind").Description, 13, ink)))
		action("rebind", "Rebind PTT", saving || s.binding != nil, func() { s.rebind(draft, w) })
		section("Developer", "Debug mode opens a console and writes a debug.log file")
		toggle("debug_mode", "Debug Mode")
		toggle("simulation_mode", "Simulation mode")
		toggle("ignore_speed_restriction", "Ignore speed restriction")
		pick("force_accent", "Force accent", append([]string{"Disabled"}, accents...))
		body = append(body, gui.Row(gui.ContainerCfg{
			Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, VAlign: gui.VAlignMiddle,
			Content: []gui.View{text("Built with ♥ by ziul510", 11, muted), spacer(), button("about", "About", 0, false, false, s.about)},
		}))
		save := buttonConfig("save", "Save Changes", 130, true, saving || closePresets != nil || s.binding != nil, func() {
			message = "Saving…"
			saving = true
			next := s.mergeSettings(base, draft)
			s.save(next, func(err error) {
				saving = false
				if err != nil {
					message = err.Error()
					w.InvalidateLayout()
					return
				}
				closeSettings()
				w.Close()
				s.main.InvalidateLayout()
			})
		})
		save.Height = 28
		scrollPadding := gui.NoPadding
		scrollPadding.Right = 16
		return gui.Column(gui.ContainerCfg{
			Sizing: gui.FillFill, Color: panel, Padding: gui.PadVH(0, 16), Spacing: gui.NoSpacing, SizeBorder: gui.NoBorder,
			Content: []gui.View{
				gap(1, 16), labelRow(bold("Settings", 18, ink), 28), gap(1, 2),
				labelRow(wrapped("Configure audio, connection, speech, presets, and control preferences", 11, muted), 28), gap(1, 12),
				gui.Column(gui.ContainerCfg{
					ID: "settings-scroll", Sizing: gui.FillFill, Scrollable: true, ScrollMode: gui.ScrollVerticalOnly, ScrollbarCfgY: &slimScrollbar,
					Padding: scrollPadding, Spacing: gui.SpacingPx(16), SizeBorder: gui.NoBorder, Content: body,
				}),
				gap(1, 8),
				gui.Row(gui.ContainerCfg{
					Sizing: gui.FillFixed, Height: 44, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, VAlign: gui.VAlignMiddle,
					Content: []gui.View{wrapped(message, 11, muted), spacer(), gui.Button(save)},
				}),
			},
		})
	}
	message = "Changes applied on save"
	s.dialog(gui.WindowCfg{Title: "Settings", Width: 520, Height: 820, MinWidth: 480, MinHeight: 680}, closeSettings, view)
	go func() {
		in, out := deviceNames(true), deviceNames(false)
		s.async(func() { inputs, outputs = in, out; s.host.Broadcast(func(w *gui.Window) { w.InvalidateLayout() }) })
	}()
}

func (s *state) about() {
	if s.aboutOpen {
		return
	}
	s.aboutOpen = true
	contents := strings.TrimSpace(readNotice("LICENSE") + "\n\n" + readNotice("THIRD_PARTY_NOTICES"))
	close := func() { s.aboutOpen = false }
	s.dialog(gui.WindowCfg{Title: "About GSXComms", Width: 640, Height: 520}, close, func(w *gui.Window) gui.View {
		return page("notices", []gui.View{bold("GSXComms", 22, ink), text("Copyright 2026 ziul510", 12, muted)}, []gui.View{wrapped(contents, 12, ink)}, []gui.View{spacer(), button("close", "Close", 80, true, false, func() { close(); w.Close() })})
	})
}

func (s *state) openAccents() {
	if s.accentOpen {
		return
	}
	s.accentOpen = true
	close := func() { s.accentOpen = false }
	s.dialog(gui.WindowCfg{Title: "GSXComms Accent Setup", Width: 720, Height: 600, MinWidth: 520, MinHeight: 420}, close, func(w *gui.Window) gui.View {
		var rows []gui.View
		for _, pack := range packs {
			rows = append(rows, gui.Column(outlinedCard(panel, gui.PadVH(12, 14), 4,
				labelRow(bold(pack.name, 14, ink), 22), labelRow(wrapped("Includes: "+strings.Join(pack.accents, ", "), 12, muted), 24))))
		}
		list := outlinedCard(card, gui.PadVH(12, 16), 12, rows...)
		list.ID, list.Sizing, list.Scrollable = "accent-packs", gui.FillFill, true
		list.ScrollMode, list.ScrollbarCfgY = gui.ScrollVerticalOnly, &slimScrollbar
		list.Padding.Right = 26
		done := buttonConfig("continue", "Done", 140, true, false, func() {
			next := s.settingsDraft()
			next.Put("accent_setup_complete", true)
			s.save(next, nil)
			close()
			w.Close()
		})
		done.Height = 40
		padding := gui.PadVH(30, 40)
		padding.Top = 40
		return gui.Column(gui.ContainerCfg{
			Sizing: gui.FillFill, Color: bg, Padding: padding, Spacing: gui.SpacingPx(20), SizeBorder: gui.NoBorder,
			Content: []gui.View{
				gui.Column(gui.ContainerCfg{
					Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, Spacing: gui.SpacingPx(8),
					Content: []gui.View{bold("Accent Setup", 24, ink), wrapped("Accent archives are unavailable in this build.", 13, muted)},
				}),
				labelRow(text("Regional accent packs", 13, muted), 28), gui.Column(list),
				gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, Content: []gui.View{spacer(), gui.Button(done)}}),
			},
		})
	})
}

func (s *state) showHotfix() {
	if s.hotfixOpen || config.Get[bool](s.cfg, "gsx_hotfix_completed") {
		return
	}
	s.hotfixOpen = true
	close := func() { s.hotfixOpen = false }
	s.dialog(gui.WindowCfg{Title: "GSX hotfix required", Width: 470, Height: 250}, close, func(w *gui.Window) gui.View {
		return page("hotfix-text", []gui.View{bold("GSX hotfix required", 18, ink)}, []gui.View{wrapped("Please select the second option in the GSX menu, apply the hotfixes, and restart GSXComms", 14, ink)}, []gui.View{
			spacer(), button("hotfix-close", "Close", 0, false, false, func() { close(); w.Close() }),
			button("hotfix-ack", "I applied the hotfix", 0, true, false, func() {
				next := s.settingsDraft()
				next.Put("gsx_hotfix_completed", true)
				s.save(next, nil)
				close()
				w.Close()
			}),
		})
	})
}
