// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
)

func (s *state) openPresets(parent *gui.Window, onClose func()) func() {
	// Reopening can overlap the previous editor's pending write.
	entries := config.Get[[]config.Preset](s.settingsDraft(), "presets")
	form := config.Preset{Enabled: true}
	selected, expanded := -1, -1
	editing, saving := false, false
	message := ""
	available := config.Services[0]
	startNew := func() {
		selected, form, message = -1, config.Preset{Enabled: true}, ""
		editing = true
	}
	var window *gui.Window
	persist := func(updated []config.Preset) {
		saving, message = true, ""
		s.saveSetting("presets", updated, func(err error) {
			saving = false
			if err != nil {
				message = err.Error()
			} else {
				entries = updated
				selected, expanded, editing = -1, -1, false
				if !parent.CloseRequested() {
					parent.InvalidateLayout()
				}
			}
			if window != nil && !window.CloseRequested() {
				window.InvalidateLayout()
			}
		})
	}
	save := func() {
		// Check duplicates against every other draft entry before inserting or replacing one
		value, err := config.ValidatePreset(form, entries, selected)
		if err != nil {
			message = err.Error()
			return
		}
		updated := slices.Clone(entries)
		if selected >= 0 {
			updated[selected] = value
		} else {
			updated = append(updated, value)
		}
		persist(updated)
	}
	var closeDialog func()
	closeDialog = s.dialog(gui.WindowCfg{Title: "Preset Manager", Width: 560, Height: 680, MinWidth: 520, MinHeight: 560}, onClose, func(w *gui.Window) gui.View {
		window = w
		var body []gui.View
		for i, p := range entries {
			id := strconv.Itoa(i)
			count := "1 service"
			if len(p.Services) != 1 {
				count = fmt.Sprintf("%d services", len(p.Services))
			}
			color := ink
			if !p.Enabled {
				color = muted
				count += " | Disabled"
			}
			action := "Expand"
			if expanded == i {
				action = "Collapse"
			}
			entry := buttonConfig("preset-"+id, action+" "+p.Name, 0, false, saving, func() {
				if expanded == i {
					expanded = -1
				} else {
					expanded = i
				}
			})
			entry.Sizing, entry.Height, entry.HAlign = gui.FillFit, 0, gui.Some(gui.HAlignLeft)
			entry.Padding = gui.PadVH(10, 12)
			entry.A11YCfg.A11YLabel = action + " " + p.Name
			entry.Content = []gui.View{wrapped(p.Name+" | "+p.Codeword+" | "+count, 13, color)}
			rows := []gui.View{gui.Button(entry)}
			if expanded == i {
				for index, service := range p.Services {
					rows = append(rows, wrapped(fmt.Sprintf("%d. %s", index+1, service), 13, color))
				}
				rows = append(rows, button("edit-preset-"+id, "Edit", 0, false, saving, func() {
					selected, form, message, editing = i, p, "", true
					form.Services = slices.Clone(p.Services)
				}))
			}
			body = append(body, gui.Column(outlinedCard(card, gui.PadAll(8), 5, rows...)))
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
				toggleControl("preset-enabled", "Enabled", controlTextSize, form.Enabled, func(ctx gui.EventCtx) {
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
				actions = append(actions, button("delete", "Delete", 0, false, saving, func() {
					index := selected
					w.NativeConfirmDialog(gui.NativeConfirmDialogCfg{
						Title: "Delete preset?", Body: "Delete " + entries[index].Name + "?", Level: gui.AlertWarning,
						OnDone: func(result gui.NativeAlertResult, _ *gui.Window) {
							if result.Status == gui.DialogOK && !saving && index == selected {
								persist(slices.Delete(slices.Clone(entries), index, index+1))
							}
						},
					})
				}))
			}
			actions = append(actions, spacer(), button("cancel-edit", "Cancel", 0, false, saving, func() { editing = false }),
				button("save-preset", "Save preset", 100, true, saving, save))
			editor = append(editor, gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, Spacing: gui.SpacingPx(8), SizeBorder: gui.NoBorder, Content: actions}))
			editorCard := outlinedCard(card, gui.PadAll(12), 10, editor...)
			editorCard.ColorBorder = accent
			// Insert below the selected row, or at the top when creating a preset
			body = slices.Insert(body, selected+1, gui.Column(editorCard))
		}
		newPreset := buttonConfig("new", "+ New", 96, true, saving, func() {
			startNew()
			w.ScrollVerticalTo("presets-scroll", 0)
		})
		newPreset.Height = 34
		header := gui.Row(gui.ContainerCfg{
			Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder, VAlign: gui.VAlignMiddle,
			Content: []gui.View{
				gui.Column(gui.ContainerCfg{
					Sizing: gui.FillFit, Padding: gui.NoPadding, Spacing: gui.SpacingPx(5), SizeBorder: gui.NoBorder,
					Content: []gui.View{bold("Presets", 20, ink), wrapped("Expand a preset to see its services", 11, muted)},
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
