// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/models"
)

const (
	controlTextSize  = 14
	settingsTextSize = 12
)

// Use the Windows UI font for app labels and controls
func labelStyle(size float32, color gui.Color) gui.TextStyle {
	return gui.TextStyle{Size: size, Color: color, Family: "Segoe UI"}
}

// A plain label suits compact text that fits in a single row
func text(label string, size float32, color gui.Color) gui.View {
	return gui.Label(label, labelStyle(size, color))
}

// Keep headings and status text on the same typography as plain labels
func bold(label string, size float32, color gui.Color) gui.View {
	return gui.Label(label, labelStyle(size, color).Bold())
}

// Use a wrapping text view for longer strings that have to fit a narrow window
func wrapped(label string, size float32, color gui.Color) gui.View {
	return gui.Text(gui.TextCfg{Text: label, Mode: gui.TextModeWrap, TextStyle: labelStyle(size, color)})
}

func buttonConfig(id, label string, width float32, primary, disabled bool, onClick func()) gui.ButtonCfg {
	return sizedButtonConfig(id, label, width, controlTextSize, primary, disabled, onClick)
}

func sizedButtonConfig(id, label string, width, size float32, primary, disabled bool, onClick func()) gui.ButtonCfg {
	style := labelStyle(size, ink)
	colors := gui.ColorSet{Base: card, Hover: elevated}
	// Primary controls use the accent fill and dark text for contrast
	if primary {
		style = labelStyle(size, bg).Bold()
		colors = gui.ColorSet{Base: accent, Hover: accentHover}
	}
	sizing := gui.FitFixed
	// A zero width lets buttons size themselves to their labels
	if width > 0 {
		sizing = gui.FixedFixed
	}
	return gui.ButtonCfg{
		ID: id, Width: width, Height: 36, Sizing: sizing, Radius: gui.RadiusPx(6), Padding: gui.PadVH(0, 8),
		SizeBorder: gui.NoBorder, Colors: colors, Disabled: disabled, VAlign: gui.Some(gui.VAlignMiddle),
		Content: []gui.View{gui.Label(label, style)},
		// Consume the click and request a redraw after the callback changes app state
		OnClick: func(ctx gui.EventCtx) { ctx.Consume(); onClick(); ctx.Window.InvalidateLayout() },
	}
}

// Use the shared button defaults when the caller needs no layout overrides
func button(id, label string, width float32, primary, disabled bool, onClick func()) gui.View {
	return gui.Button(buttonConfig(id, label, width, primary, disabled, onClick))
}

// A fill-width spacer pushes later controls to the far edge of a row
func spacer() gui.View {
	return gui.Rectangle(gui.RectangleCfg{Sizing: gui.FillFixed, Height: 1})
}

// Use a fixed-size gap when the layout needs stable spacing
func gap(width, height float32) gui.View {
	return gui.Rectangle(gui.RectangleCfg{Sizing: gui.FixedFixed, Width: width, Height: height})
}

var slimScrollbar = gui.ScrollbarCfg{
	Size: 8, RadiusThumb: 4, GapEdge: gui.SomeF(4), GapEnd: gui.SomeF(4),
	ColorThumb: gui.Hex(0x6e6e6e), ColorBackground: gui.ColorTransparent,
}

func mainView(w *gui.Window) gui.View {
	s := gui.State[state](w)
	// The status bar distinguishes the SimConnect connection from Couatl readiness
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
	// Show a placeholder until a valid speed is available Warn visually above the PTT limit
	speed, speedColor := "-- kts", muted
	if reading := s.groundSpeed.Load(); s.connected && reading.fresh() {
		speed, speedColor = fmt.Sprintf("%d kts", int(reading.knots)), accent
		if reading.knots > 50 {
			speedColor = orange
		}
	}
	bar := []gui.View{bold("● "+status, 12, statusColor), bold("● GSX", 12, gsxColor), spacer()}
	// The settings can hide the topmost control without resetting its active value
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
			topmost.Content = []gui.View{text("Top Most", controlTextSize, muted)}
			topmost.Colors = gui.ColorSet{Base: elevated, Hover: card, Border: bg, BorderFocus: accent}
		}
		bar = append(bar, gui.Button(topmost), gap(4, 1))
	}
	bar = append(bar, bold(speed, 12, speedColor))
	ptt, pttColor := "PTT Idle", ink
	binding, bound := s.bindingDescription(config.Get[config.Binding](s.cfg, "ptt_bind"))
	bindingColor := muted
	// In-progress activity takes priority over readiness for a new recording
	switch {
	case s.ptt == "LISTENING":
		ptt, pttColor = "PTT LISTENING", accent
	case s.ptt == "PROCESSING":
		ptt, pttColor = "PTT PROCESSING", orange
	case !bound || !s.pttAvailable():
		ptt, pttColor = "Unavailable", red
		if s.speechStatus == "Speech unavailable" {
			binding, bindingColor = "Speech recognition unavailable", red
		} else if s.speechStatus != "" {
			binding, bindingColor = s.speechStatus, orange
		}
	}
	body := []gui.View{gui.Row(gui.ContainerCfg{
		Sizing: gui.FillFixed, Height: 48, Color: card, ColorBorder: elevated, Radius: gui.RadiusPx(6),
		Padding: gui.PadVH(0, 11), SizeBorder: gui.BorderPx(1), VAlign: gui.VAlignMiddle, Spacing: gui.SpacingPx(6),
		Content: []gui.View{bold(ptt, 13, pttColor), spacer(), text(binding, 11, bindingColor)},
	})}
	// Only show the command field when typed input is enabled
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
						// Clear the field before routing the saved text through the normal command path
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
	// Use the conversation space for the live menu while keeping the surrounding controls in place
	if s.overlay {
		logArea.ColorBorder, logArea.Padding = accent, gui.PadAll(8)
		title := s.menu.Title
		if title == "" {
			title = "GSX Menu"
		}
		logArea.Content = append(logArea.Content, bold(title, 15, ink))
		// The overlay can remain open while the next panel file is still empty
		if len(s.menu.Options) == 0 {
			logArea.Content = append(logArea.Content, text("No GSX menu available", controlTextSize, muted))
		}
		for i, item := range s.menu.Options {
			// Display one-based labels but pass the original zero-based slot back to GSX
			label := fmt.Sprintf("%d. %s", i+1, item)
			fill, fg := card, ink
			for _, highlight := range s.highlights {
				if highlight.index == i && highlight.page.Same(s.menu) && w.Now().Before(highlight.until) {
					fill, fg = accent, bg
					break
				}
			}
			logArea.Content = append(logArea.Content, gui.Row(gui.ContainerCfg{
				Sizing: gui.FillFit, Color: fill, Radius: gui.RadiusPx(4), Padding: gui.PadVH(7, 16),
				Content: []gui.View{wrapped(label, controlTextSize, fg)},
			}))
		}
	} else {
		logArea.Content = []gui.View{gui.Text(gui.TextCfg{ID: "log-content", Text: strings.Join(s.lines, "\n"), Mode: gui.TextModeWrap, TextStyle: gui.TextStyle{Size: 12, Family: "Consolas", Color: terminalInk, LetterSpacing: 0.4}, Focusable: true})}
	}
	body = append(body, gui.Column(logArea))
	connect := buttonConfig("connect", "Connect", 120, true, s.busy || s.connected, s.toggleConnect)
	switch {
	case s.busy:
		connect.Content = []gui.View{bold("Connecting…", controlTextSize, bg)}
		connect.Colors = gui.ColorSet{Base: orange, Disabled: orange}
	case s.connected:
		connect.Content = []gui.View{bold("Connected", controlTextSize, bg)}
		connect.Colors = gui.ColorSet{Base: green, Disabled: green}
	}
	connect.Height = 36
	menuLabel := "Show GSX Menu"
	if s.overlay {
		menuLabel = "Hide GSX Menu"
	}
	menu := buttonConfig("gsx-menu", menuLabel, 120, false, !s.ready && !s.overlay, s.toggleMenu)
	settings := buttonConfig("settings", "Settings", 80, false, false, s.openSettings)
	for _, control := range []*gui.ButtonCfg{&menu, &settings} {
		control.Height = 36
		control.Colors = gui.ColorSet{Base: gui.ColorTransparent, Hover: elevated, Disabled: gui.ColorTransparent}
	}
	menu.Content = []gui.View{text(menuLabel, controlTextSize, muted)}
	settings.Content = []gui.View{text("Settings", controlTextSize, muted)}
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
	s.dialogID++
	id := s.dialogID
	if s.dialogs == nil {
		s.dialogs = make(map[uint64]func())
	}
	var window *gui.Window
	closed := false
	closeDialog := func() {
		// Run cleanup once even if both the parent and the window request closure
		if !closed {
			closed = true
			delete(s.dialogs, id)
			onClose()
		}
		if window != nil {
			window.Close()
		}
	}
	cfg.IconPNG = s.icon
	onInit := cfg.OnInit
	cfg.OnInit = func(w *gui.Window) {
		window = w
		// The close callback can run before go-gui creates the queued window
		if closed {
			w.Close()
			return
		}
		w.TitlebarDark(true)
		w.SetView(view)
		if onInit != nil {
			onInit(w)
		}
	}
	cfg.OnCloseRequest = func(*gui.Window) { closeDialog() }
	s.dialogs[id] = closeDialog
	s.host.OpenWindow(cfg)
	return closeDialog
}

// Scroll the middle panel while keeping the title and actions visible
func page(id string, header, body, footer []gui.View) gui.View {
	scrollPadding := gui.NoPadding
	scrollPadding.Right = 16
	return gui.Column(gui.ContainerCfg{Sizing: gui.FillFill, Color: bg, Padding: gui.PadAll(20), Spacing: gui.SpacingPx(12), SizeBorder: gui.NoBorder, Content: []gui.View{
		gui.Column(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, Spacing: gui.SpacingPx(4), SizeBorder: gui.NoBorder, Content: header}),
		gui.Column(gui.ContainerCfg{ID: id, Sizing: gui.FillFill, Scrollable: true, ScrollbarCfgY: &slimScrollbar, ScrollMode: gui.ScrollVerticalOnly, Padding: scrollPadding, Spacing: gui.SpacingPx(8), SizeBorder: gui.NoBorder, Content: body}),
		gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, VAlign: gui.VAlignMiddle, Spacing: gui.SpacingPx(8), SizeBorder: gui.NoBorder, Content: footer}),
	}})
}

// Let cards grow with their contents and use the common outline treatment
func outlinedCard(fill gui.Color, padding gui.Padding, spacing float32, content ...gui.View) gui.ContainerCfg {
	return gui.ContainerCfg{
		Sizing: gui.FillFit, Color: fill, ColorBorder: elevated, SizeBorder: gui.BorderPx(1),
		Radius: gui.RadiusPx(6), Padding: padding, Spacing: gui.SpacingPx(spacing), Content: content,
	}
}

// Align short labels vertically within the fixed space reserved for their rows
func labelRow(label gui.View, height float32) gui.View {
	return gui.Row(gui.ContainerCfg{
		Sizing: gui.FillFixed, Height: height, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, VAlign: gui.VAlignMiddle,
		Content: []gui.View{label},
	})
}

// Place the field caption over a control that can use the full column width
func settingsField(label string, control gui.View) gui.View {
	return gui.Column(gui.ContainerCfg{
		Sizing: gui.FillFit, Padding: gui.NoPadding, Spacing: gui.SpacingPx(6), SizeBorder: gui.NoBorder,
		Content: []gui.View{labelRow(text(label, settingsTextSize, muted), 28), control},
	})
}

type fullWidthTooltip struct{ gui.View }

func (v fullWidthTooltip) GenerateLayout(w *gui.Window) gui.Layout {
	layout := gui.GenerateViewLayout(v.View, w)
	layout.Shape.Sizing = gui.FillFit
	layout.Shape.Padding = gui.NoPadding
	return layout
}

// Apply shared field styling while preserving the caller's value and callbacks
func formInput(cfg gui.InputCfg) gui.View {
	if cfg.TextStyle == (gui.TextStyle{}) {
		cfg.TextStyle = labelStyle(controlTextSize, ink)
	}
	cfg.Sizing = gui.FillFit
	cfg.Padding, cfg.Radius, cfg.SizeBorder = gui.PadVH(6, 7), gui.RadiusPx(5), gui.BorderPx(1)
	cfg.Colors = gui.ColorSet{Base: elevated, Border: bg, BorderFocus: accent}
	return gui.Input(cfg)
}

// Keep dropdown styling consistent even when dialogs supply different choices
func formSelect(cfg gui.SelectCfg) gui.View {
	if cfg.TextStyle == (gui.TextStyle{}) {
		cfg.TextStyle = labelStyle(controlTextSize, ink)
	}
	cfg.Sizing = gui.FillFit
	cfg.Padding, cfg.Radius, cfg.SizeBorder = gui.PadVH(7, 7), gui.RadiusPx(5), gui.NoBorder
	cfg.Colors = gui.ColorSet{Base: card, Hover: elevated, BorderFocus: accent}
	return gui.Select(cfg)
}

func toggleControl(id, label string, size float32, selected bool, onClick func(gui.EventCtx)) gui.View {
	// Use the passed-in value so toggles can show draft edits before Save
	track := disabledInk
	if selected {
		track = accent
	}
	return gui.Switch(gui.SwitchCfg{
		ID: id, Label: label, Selected: selected, TextStyleLabel: labelStyle(size, ink),
		Width: gui.SomeF(34), Height: gui.SomeF(18), SizeBorder: gui.BorderPx(1), Padding: gui.NoPadding,
		ColorSelect: ink, ColorUnselect: ink, Colors: gui.ColorSet{Base: track, Border: track, BorderFocus: ink},
		OnClick: onClick,
	})
}

func (s *state) settingsDraft() config.Values {
	// Base new edits on the latest queued save while disk writes are pending
	if s.pendingSave != nil {
		return s.pendingSave.values.Copy()
	}
	return s.cfg.Copy()
}

func (s *state) mergeSettings(base, draft config.Values) config.Values {
	next := s.settingsDraft()
	// Keep newer saves for fields this dialog didn't edit
	for name, value := range draft {
		if !bytes.Equal(base[name], value) {
			next[name] = bytes.Clone(value)
		}
	}
	return next
}

func (s *state) save(next config.Values, then func(error)) {
	next.RemoveDisabledReadMenu()
	req := &saveRequest{next, then}
	select {
	// Only advertise a pending draft once the writer has accepted it
	case s.saves <- req:
		s.pendingSave = req
	default:
		// Report a full queue through the dialog callback or the main conversation
		err := errors.New("settings save queue is busy")
		s.traceLevel("WARN", "SYS", "Settings save skipped because the queue is full")
		if then != nil {
			then(err)
		} else {
			s.log("ERR", err.Error())
		}
	}
}

func (s *state) openSettings() {
	if s.settingsOpen {
		focusWindow("Settings")
		return
	}
	s.settingsOpen = true
	// Keep an opening snapshot so Save can tell which fields this dialog changed
	base := s.settingsDraft()
	draft := base.Copy()
	key, _ := config.ReadKey()
	setup := speechSetup{hasKey: key != "", tutorialDir: assetDir("tutorial"), local: map[string]localModelStatus{}}
	for _, id := range []string{"parakeet", "orukeet", "kokoro"} {
		model, _ := models.Lookup(id)
		setup.local[id] = localModelStatus{ready: model.Ready(s.dir), cached: model.Cached(s.dir)}
	}
	s.speechChanged = func(id string) {
		if model, ok := models.Lookup(id); ok {
			setup.local[id] = localModelStatus{ready: model.Ready(s.dir), cached: model.Cached(s.dir)}
		}
	}
	var message string
	var saving, closed bool
	var readerSaving bool
	var closePresets func()
	var closeWindow func()
	closeSettings := func() {
		if !closed {
			closed = true
			if setup.cancel != nil {
				setup.cancel()
			}
			setup.step, setup.key = 0, ""
			s.settingUpMistral = false
			// Cancel any pending input capture when leaving Settings
			if s.binding != nil {
				s.binding(config.Binding{})
			}
			// Close the child editor with Settings.
			if closePresets != nil {
				closePresets()
			}
			s.settingsOpen = false
			s.speechChanged = nil
		}
	}
	var inputs, outputs []audioDevice
	help := map[string]string{
		"enable_menu_matching":             "Use GSX menu labels to correct and match unrecognized commands.",
		"mute_gsx_audio_during_crew_voice": "Mute Couatl while GSXComms speaks or sends a service command.",
		"simulation_mode":                  "Allow speech testing while Microsoft Flight Simulator is disconnected.",
		"ignore_speed_restriction":         "Allow transmissions above 50 knots.",
	}
	view := func(w *gui.Window) gui.View {
		if setup.step > 0 {
			return setup.mistralPage(s, w)
		}
		var body []gui.View
		// Let section cards grow when subtitles wrap
		section := func(name, description string) {
			body = append(body, gui.Column(outlinedCard(card, gui.PadVH(12, 11), 4,
				bold(name, 15, ink), wrapped(description, 13, muted))))
		}
		// Control changes update the draft immediately Disk writes wait for Save
		toggle := func(key, label string) {
			control := toggleControl(key, label, settingsTextSize, config.Get[bool](draft, key), func(ctx gui.EventCtx) {
				draft.Put(key, !config.Get[bool](draft, key))
				ctx.Consume()
				ctx.Window.InvalidateLayout()
			})
			if hint := help[key]; hint != "" {
				control = fullWidthTooltip{gui.WithTooltip(w, gui.WithTooltipCfg{ID: key + "-help", Text: hint, Content: []gui.View{control}})}
			}
			body = append(body, labelRow(control, 24))
		}
		pick := func(key, label string, items []string) {
			value := config.Get[string](draft, key)
			// Preserve saved choices even when the available list changes
			if !slices.Contains(items, value) && value != "" {
				items = append(slices.Clone(items), value)
			}
			body = append(body, settingsField(label, formSelect(gui.SelectCfg{
				ID: key, A11YCfg: gui.A11YCfg{A11YLabel: label}, Items: items, Selected: []string{value},
				TextStyle: labelStyle(settingsTextSize, ink),
				OnSelect: func(values []string, ctx gui.EventCtx) {
					if len(values) > 0 {
						draft.Put(key, values[0])
					}
					ctx.Consume()
				}})))
		}
		pickAudio := func(prefix, label string, devices []audioDevice) {
			nameKey, idKey := prefix+"_device_name", prefix+"_device_id"
			name := config.Get[string](draft, nameKey)
			endpoint := config.Get[string](draft, idKey)
			allowFallback := prefix != "input" || config.Get[string](draft, "speech_provider") != "mistral"
			id := matchAudioDevice(name, endpoint, devices, allowFallback)
			items := []string{"Default"}
			choices := map[string]audioDevice{"Default": {name: "Default"}}
			addChoice := func(label string, device audioDevice) string {
				option := label
				for suffix := 2; ; suffix++ {
					if _, exists := choices[option]; !exists {
						break
					}
					option = fmt.Sprintf("%s (%d)", label, suffix)
				}
				items = append(items, option)
				choices[option] = device
				return option
			}
			selected := "Default"
			for i, device := range devices {
				if device.name == "" {
					continue
				}
				option := addChoice(device.name, device)
				if uintptr(i) == id {
					selected = option
				}
			}
			if id == defaultWaveDevice && !defaultAudioSelection(name, endpoint) {
				displayName := name
				if displayName == "" {
					displayName = label
				}
				selected = addChoice(displayName+" (unavailable)", audioDevice{name, endpoint})
			}
			body = append(body, settingsField(label, formSelect(gui.SelectCfg{
				ID: nameKey, A11YCfg: gui.A11YCfg{A11YLabel: label}, Items: items, Selected: []string{selected},
				TextStyle: labelStyle(settingsTextSize, ink),
				OnSelect: func(values []string, ctx gui.EventCtx) {
					if len(values) > 0 {
						if choice, ok := choices[values[0]]; ok {
							draft.Put(nameKey, choice.name)
							draft.Put(idKey, choice.endpoint)
						}
					}
					ctx.Consume()
				},
			})))
		}
		slider := func(key, label string, minv, maxv, step float32) {
			control := gui.Slider(gui.SliderCfg{
				ID: key, A11YCfg: gui.A11YCfg{A11YLabel: label}, Min: minv, Max: maxv, Step: step,
				Value: float32(config.Get[float64](draft, key)), Sizing: gui.FillFixed, Height: 16,
				Size: 4, ThumbSize: 16, SizeBorder: gui.NoBorder, Radius: gui.NoRadius,
				Padding: gui.NoPadding, Color: bg, ColorLeft: accent, ColorThumb: ink,
				OnChange: func(v float32, ctx gui.EventCtx) {
					// The capture limit is read back from settings as an integer
					if step == 1 {
						draft.Put(key, int(v))
					} else {
						draft.Put(key, v)
					}
					ctx.Consume()
					ctx.Window.InvalidateLayout()
				},
			})
			value := fmt.Sprintf("%.0f%%", config.Get[float64](draft, key)*100)
			if step == 1 {
				value = fmt.Sprintf("%d s", config.Get[int](draft, key))
			}
			body = append(body, settingsField(label, fullWidthTooltip{gui.WithTooltip(w, gui.WithTooltipCfg{
				ID: key + "-value", Text: value,
				Content: []gui.View{gui.Row(gui.ContainerCfg{
					Sizing: gui.FillFixed, Height: 16, Padding: gui.PadVH(0, 6), SizeBorder: gui.NoBorder,
					Content: []gui.View{control},
				})},
			})}))
		}
		action := func(id, label string, disabled bool, onClick func()) {
			control := sizedButtonConfig(id, label, 0, settingsTextSize, false, disabled, onClick)
			control.Sizing = gui.FillFixed
			body = append(body, gui.Button(control))
		}
		section("Audio Devices", "Choose preferred input and output paths")
		pickAudio("input", "Microphone", inputs)
		pickAudio("output", "Speaker", outputs)
		section("Audio Mix", "Tune static and crew voice levels")
		toggle("enable_static", "Enable radio static")
		slider("volume_static", "Static volume", 0, 0.5, 0.001)
		slider("volume_voice", "Voice volume", 0, 1, 0.001)
		slider("max_recording_seconds", "Max recording (seconds)", 10, 120, 1)
		section("Speech Recognition", "Pick a local model or the cloud Voxtral Small model")
		body = append(body, gui.Column(gui.ContainerCfg{
			Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, Spacing: gui.SpacingPx(10),
			Content: []gui.View{
				setup.localCard(s, w, "parakeet", "Standard, Recommended (Parakeet v2 0.6b)"),
				setup.localCard(s, w, "orukeet", "Enhanced (Orukeet)"),
				setup.mistralCard(s, w),
			},
		}))
		if setup.providerError != "" {
			body = append(body, wrapped(setup.providerError, 12, red))
		}
		section("Accent", "Default accent and regional packs")
		var selectedAccents []string
		enabled := config.Get[[]string](s.cfg, "installed_packs")
		for _, pack := range packs {
			if slices.Contains(enabled, pack.name) {
				selectedAccents = append(selectedAccents, pack.accents...)
			}
		}
		slices.Sort(selectedAccents)
		pick("default_accent", "Default accent", selectedAccents)
		action("accent-setup", "Open Accent Setup", false, s.openAccents)
		section("Presets", "Bundle multiple services behind a codeword and pick the run order")
		presets := config.Get[[]config.Preset](s.cfg, "presets")
		summary := "No presets yet. Open Preset Manager to create one."
		if len(presets) == 1 {
			summary = "1 preset"
		} else if len(presets) > 1 {
			summary = fmt.Sprintf("%d presets", len(presets))
		}
		body = append(body, text(summary, 11, muted))
		action("presets", "Open Preset Manager", closePresets != nil || saving, func() {
			closePresets = s.openPresets(w, func() { closePresets = nil; w.InvalidateLayout() })
		})
		section("Behavior", "The behavior of the app")
		toggle("enable_menu_matching", "Broader Mode")
		const readKey = config.MenuReadoutSetting
		readerEnabled := config.Get[bool](s.cfg, readKey)
		readerDownloading := setup.running == "kokoro"
		body = append(body, labelRow(toggleControl(readKey, "Read GSX menus aloud after a keyword", settingsTextSize, readerEnabled || readerDownloading, func(ctx gui.EventCtx) {
			ctx.Consume()
			if readerSaving {
				return
			}
			setEnabled := func(enabled bool) {
				readerSaving = true
				s.saveSetting(readKey, enabled, func(err error) {
					readerSaving = false
					if err != nil {
						message = err.Error()
					}
					if !closed {
						w.InvalidateLayout()
					}
				})
			}
			if readerDownloading {
				setup.cancel()
			} else if readerEnabled {
				setEnabled(false)
			} else if setup.local["kokoro"].ready {
				setEnabled(true)
			} else if setup.running != "" {
				status := setup.local["kokoro"]
				status.message = "Wait for the speech model operation to finish, then try again"
				setup.local["kokoro"] = status
			} else {
				// Failed or canceled downloads leave readout off.
				setup.runLocal(s, w, "kokoro", "Downloading", func(err error) {
					if err == nil && !closed {
						setEnabled(true)
					}
				})
			}
			w.InvalidateLayout()
		}), 24))
		// Show the keyword field only when menu readout is enabled
		if readerEnabled {
			body = append(body, formInput(gui.InputCfg{
				ID: "menu-reader-keyword", Label: "Keyword:", Placeholder: "e.g. read menu", TextStyle: labelStyle(settingsTextSize, ink),
				Text: config.Get[string](draft, "gsx_menu_reader_keyword"), OnTextChanged: func(v string, _ gui.EventCtx) { draft.Put("gsx_menu_reader_keyword", v) },
			}))
		} else if note := setup.local["kokoro"].message; note != "" {
			body = append(body, wrapped(note, 11, muted))
		}
		toggle("auto_connect_sim", "Auto-connect to simulator")
		toggle("auto_select_operator", "Auto-select operator")
		toggle("mute_gsx_audio_during_crew_voice", "Mute Couatl/GSX during speech")
		section("Controls", "PTT binding, live input handling and typing")
		toggle("enable_keyboard_input", "Enable keyboard typing")
		toggle("minimize_to_tray", "Minimize to tray")
		if config.Get[bool](draft, "minimize_to_tray") {
			toggle("start_minimized", "Start minimized")
		}
		toggle("show_topmost", "Show Top Most")
		binding := config.Get[config.Binding](s.cfg, "ptt_bind")
		description, _ := s.bindingDescription(binding)
		body = append(body, settingsField("Current PTT binding", wrapped(description, settingsTextSize, ink)))
		if strings.Contains(description, "Rebind required") && binding.Description != "" {
			body = append(body, wrapped("Needs rebinding: "+binding.Description, 12, orange))
		}
		action("rebind", "Rebind PTT", saving || s.binding != nil, func() { s.rebind(w) })
		section("Developer", "Debug mode shows diagnostics in your default terminal and saves debug.log in the app’s data folder")
		toggle("debug_mode", "Debug Mode")
		toggle("simulation_mode", "Simulation mode")
		toggle("ignore_speed_restriction", "Ignore speed restriction")
		action("randomize-gender", "Randomize gender", false, s.randomizeVoice)
		// Offer accents from the same installed packs that playback uses
		pick("force_accent", "Force accent", append([]string{"Disabled"}, selectedAccents...))
		save := sizedButtonConfig("save", "Save Changes", 130, settingsTextSize, true, saving || closePresets != nil || s.binding != nil || readerDownloading, func() {
			message = "Saving…"
			saving = true
			// Merge only edits made in this window onto the latest queued settings
			next := s.mergeSettings(base, draft)
			s.save(next, func(err error) {
				saving = false
				// Leave Settings open on a write failure so the draft can be retried
				if err != nil {
					message = err.Error()
					w.InvalidateLayout()
					return
				}
				closeWindow()
				s.main.InvalidateLayout()
			})
		})
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
				gui.Row(gui.ContainerCfg{
					Sizing: gui.FillFixed, Height: 32, Padding: gui.NoPadding, SizeBorder: gui.NoBorder,
					HAlign: gui.HAlignCenter, VAlign: gui.VAlignMiddle,
					Content: []gui.View{text("Built with ♥ by ziul510", settingsTextSize, muted)},
				}),
			},
		})
	}
	message = "Changes applied on save"
	closeWindow = s.dialog(gui.WindowCfg{Title: "Settings", Width: 520, Height: 820, MinWidth: 480, MinHeight: 680,
		OnInit: func(*gui.Window) { focusWindow("Settings") }}, closeSettings, view)
	go func() {
		// Enumerating audio devices can block, so open Settings with its defaults first
		in, out := audioDevices(true), audioDevices(false)
		s.async(func() { inputs, outputs = in, out; s.host.Broadcast(func(w *gui.Window) { w.InvalidateLayout() }) })
	}()
}

func (s *state) showHotfix() {
	// Avoid duplicate prompts and respect an earlier acknowledgment
	if s.hotfixOpen || config.Get[bool](s.cfg, "gsx_hotfix_completed") {
		return
	}
	s.hotfixOpen = true
	var message string
	var saving bool
	close := func() { s.hotfixOpen = false }
	var closeWindow func()
	closeWindow = s.dialog(gui.WindowCfg{Title: "GSX hotfix required", Width: 470, Height: 250}, close, func(w *gui.Window) gui.View {
		body := []gui.View{wrapped("Please select the second option in the GSX menu, apply the hotfixes, and restart GSXComms", 14, ink)}
		if message != "" {
			body = append(body, wrapped(message, 12, red))
		}
		return page("hotfix-text", []gui.View{bold("GSX hotfix required", 18, ink)}, body, []gui.View{
			spacer(), button("hotfix-close", "Close", 0, false, saving, closeWindow),
			button("hotfix-ack", "I applied the hotfix", 0, true, saving, func() {
				next := s.settingsDraft()
				next.Put("gsx_hotfix_completed", true)
				saving = true
				s.save(next, func(err error) {
					saving = false
					if err != nil {
						message = "Could not save the acknowledgment"
						w.InvalidateLayout()
						return
					}
					closeWindow()
				})
			}),
		})
	})
}
