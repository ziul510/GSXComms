// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"slices"
	"strconv"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
)

func (s *state) openPresets(draft config.Values, parent *gui.Window, onClose func()) func() {
	// Edit the parent's draft while Settings owns the dialog
	entries := config.Get[[]config.Preset](draft, "presets")
	form := config.Preset{Enabled: true}
	selected := -1
	editing := false
	message := ""
	available := config.Services[0]
	startNew := func() {
		selected, form, message = -1, config.Preset{Enabled: true}, ""
		editing = true
	}
	save := func() {
		// Check duplicates against every other draft entry before inserting or replacing one
		value, err := config.ValidatePreset(form, entries, selected)
		if err != nil {
			message = err.Error()
			return
		}
		if selected >= 0 {
			entries[selected] = value
		} else {
			entries = append(entries, value)
		}
		// Save the edit to the Settings draft so the parent dialog controls persistence
		draft.Put("presets", entries)
		form = config.Preset{Enabled: true}
		selected = -1
		editing = false
		message = ""
		// Refresh the preset count shown in the Settings window
		parent.InvalidateLayout()
	}
	var closeDialog func()
	closeDialog = s.dialog(gui.WindowCfg{Title: "Preset Manager", Width: 560, Height: 680, MinWidth: 520, MinHeight: 560}, onClose, func(w *gui.Window) gui.View {
		var body []gui.View
		for i, p := range entries {
			entry := buttonConfig("preset-"+strconv.Itoa(i), p.Name, 0, false, false, func() {
				selected, form, message, editing = i, p, "", true
				// Editing the service order must leave the saved entry intact until Save preset
				form.Services = slices.Clone(p.Services)
			})
			entry.Sizing, entry.Height, entry.HAlign = gui.FillFit, 0, gui.Some(gui.HAlignLeft)
			entry.Padding = gui.PadVH(10, 12)
			entry.Content = []gui.View{wrapped(p.Name+" · "+p.Codeword, 13, ink)}
			body = append(body, gui.Button(entry))
		}
		// Show a first-preset action only while there is no editor to fill in
		if len(entries) == 0 && !editing {
			create := buttonConfig("create-preset", "Create a preset", 160, true, false, startNew)
			create.Height = 36
			body = append(body, gui.Column(gui.ContainerCfg{
				Sizing: gui.FillFit, HAlign: gui.HAlignCenter, Padding: gui.PadVH(40, 0), SizeBorder: gui.NoBorder, Spacing: gui.SpacingPx(14),
				Content: []gui.View{bold("No presets yet", 15, ink), wrapped("A preset bundles several GSX services behind one spoken codeword", 11, muted), gap(1, 0), gui.Button(create)},
			}))
		}
		if editing {
			title := "New preset"
			if selected >= 0 {
				title = "Edit preset"
			}
			editor := []gui.View{
				bold(title, 14, ink),
				formInput(gui.InputCfg{ID: "preset-name", Label: "Preset name", Text: form.Name, OnTextChanged: func(v string, _ gui.EventCtx) { form.Name = v }}),
				formInput(gui.InputCfg{ID: "preset-trigger", Label: "Codeword", Text: form.Codeword, OnTextChanged: func(v string, _ gui.EventCtx) { form.Codeword = v }}),
				toggleControl("preset-enabled", "Enabled", form.Enabled, func(ctx gui.EventCtx) {
					form.Enabled = !form.Enabled
					ctx.Consume()
					ctx.Window.InvalidateLayout()
				}),
				formSelect(gui.SelectCfg{ID: "preset-service", Label: "Services", Selected: []string{available}, Items: config.Services, OnSelect: func(v []string, ctx gui.EventCtx) {
					if len(v) > 0 {
						available = v[0]
					}
					ctx.Consume()
				}}),
				button("add-service", "Add service", 0, false, false, func() {
					if !slices.Contains(form.Services, available) {
						form.Services = append(form.Services, available)
					}
				}),
			}
			// The order shown here is the order GSX will receive the services
			for i, service := range form.Services {
				id := strconv.Itoa(i)
				editor = append(editor, gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, Spacing: gui.SpacingPx(8), SizeBorder: gui.NoBorder, VAlign: gui.VAlignMiddle, Content: []gui.View{
					wrapped(service, 13, ink), spacer(),
					button("up-"+id, "Up", 0, false, i == 0, func() { form.Services[i], form.Services[i-1] = form.Services[i-1], form.Services[i] }),
					button("down-"+id, "Down", 0, false, i+1 == len(form.Services), func() { form.Services[i], form.Services[i+1] = form.Services[i+1], form.Services[i] }),
					button("remove-"+id, "Remove", 0, false, false, func() { form.Services = slices.Delete(form.Services, i, i+1) }),
				}}))
			}
			if message != "" {
				editor = append(editor, wrapped(message, 11, red))
			}
			var actions []gui.View
			// Deletion is available only when the form refers to an existing preset
			if selected >= 0 {
				actions = append(actions, button("delete", "Delete", 0, false, false, func() {
					entries = slices.Delete(entries, selected, selected+1)
					draft.Put("presets", entries)
					editing = false
					parent.InvalidateLayout()
				}))
			}
			actions = append(actions, spacer(), button("cancel-edit", "Cancel", 0, false, false, func() { editing = false }),
				button("save-preset", "Save preset", 100, true, false, save))
			editor = append(editor, gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, Spacing: gui.SpacingPx(8), SizeBorder: gui.NoBorder, Content: actions}))
			editorCard := outlinedCard(card, gui.PadAll(12), 10, editor...)
			editorCard.ColorBorder = accent
			// Insert below the selected row, or at the top when creating a preset
			body = slices.Insert(body, selected+1, gui.Column(editorCard))
		}
		newPreset := buttonConfig("new", "+ New", 96, true, false, func() {
			startNew()
			w.ScrollVerticalTo("presets-scroll", 0)
		})
		newPreset.Height = 34
		header := gui.Row(gui.ContainerCfg{
			Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, VAlign: gui.VAlignMiddle,
			Content: []gui.View{
				gui.Column(gui.ContainerCfg{
					Sizing: gui.FillFit, Padding: gui.NoPadding, Spacing: gui.SpacingPx(5), SizeBorder: gui.NoBorder,
					Content: []gui.View{bold("Presets", 20, ink), wrapped("Click a preset to edit it and reorder its services", 11, muted)},
				}),
				gui.Button(newPreset),
			},
		})
		done := buttonConfig("done", "Done", 120, false, false, closeDialog)
		done.Height = 36
		return page("presets-scroll", []gui.View{header}, body, []gui.View{spacer(), gui.Button(done)})
	})
	return closeDialog
}
