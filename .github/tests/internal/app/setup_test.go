// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/geo"
	"github.com/ziul510/GSXCommsGo/internal/models"
)

func TestAccentSetupSavesPackSelection(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	s := &state{ctx: t.Context(), dir: t.TempDir(), accentsDir: "Accents",
		cfg: config.Defaults(), host: gui.NewApp(), saves: make(chan *saveRequest, 1), saved: make(chan struct{})}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	go s.settingsWriter()
	t.Cleanup(func() { close(s.saves); <-s.saved })
	s.openAccents(nil)
	w := gui.NewTestWindow(t, <-s.host.PendingOpen())
	w.TestRender(nil)
	click := func(id string) {
		t.Helper()
		if err := w.TestClick(id); err != nil {
			t.Fatal(err)
		}
	}
	save := func() config.Values {
		t.Helper()
		click("save-accents")
		waitForAccentContinue(t, w)
		click("continue-accents")
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			w.FrameFn()
			// Pump the main window too because it receives the settings writer's completion.
			s.main.FrameFn()
			if w.CloseRequested() {
				saved, err := config.Load(s.dir)
				if err != nil {
					t.Fatal(err)
				}
				return saved
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("accent setup never saved its selection")
		return nil
	}
	click("select-all")
	click("accent-packs:pack-" + packs[0].name)
	saved := config.Get[[]string](save(), "installed_packs")
	if len(saved) != len(packs)-1 || slices.Contains(saved, packs[0].name) {
		t.Fatalf("unexpected pack selection: %v", saved)
	}
	recordings := filepath.Join(s.dir, "Accents")
	for _, pack := range packs {
		if completeAccentPack(filepath.Join(recordings, pack.name), pack.accents) != slices.Contains(saved, pack.name) {
			t.Errorf("extracted packs do not match settings for %s", pack.name)
		}
	}
	if _, err := os.Stat(filepath.Join(s.dir, config.SettingsFile)); err != nil {
		t.Fatal(err)
	}
	s.openAccents(nil)
	w = gui.NewTestWindow(t, <-s.host.PendingOpen())
	w.TestRender(nil)
	click("deselect-all")
	if err := w.TestClick("save-accents"); !errors.Is(err, gui.ErrTestDisabled) {
		t.Fatalf("empty selection save is available: %v", err)
	}
	if len(s.saves) > 0 || w.CloseRequested() {
		t.Fatal("setup accepted an empty pack selection")
	}
}

func waitForAccentContinue(t *testing.T, w *gui.Window) *gui.Layout {
	t.Helper()
	// Pump window callbacks while waiting for the background install to expose Continue.
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		w.FrameFn()
		layout := w.TestRender(nil)
		if _, ok := layout.FindByID("continue-accents"); ok {
			return layout
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("accent installation never enabled Continue")
	return nil
}

func TestFirstRunCompletesOnlyAfterCreatingSettings(t *testing.T) {
	dir := t.TempDir()
	accentsDir := filepath.Join(dir, "Accents")
	if err := os.MkdirAll(accentsDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Portable installs extract recordings beside the source archive
	pack := packs[0]
	file, err := os.Create(filepath.Join(accentsDir, pack.name+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for _, accent := range pack.accents {
		for _, voice := range []string{"female", "male"} {
			entry, err := archive.Create(strings.ToLower(accent) + "/goahead/" + voice + "11.ai.mp3")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := entry.Write([]byte("fixture")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := errors.Join(archive.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	complete := false
	cfg := firstAccentWindow(t.Context(), dir, accentsDir, config.Defaults(), func(prefs config.Values) {
		complete = config.Get[bool](prefs, "accent_setup_complete")
	})
	cfg.Width, cfg.Height = cfg.MinWidth, cfg.MinHeight
	w := gui.NewTestWindow(t, cfg)
	w.TestRender(nil)
	if complete || w.CloseRequested() {
		t.Fatal("setup completed before the user saved")
	}
	if err := w.TestClick("accent-packs:pack-" + pack.name); err != nil {
		t.Fatal(err)
	}
	const pickerID = "wizard-default-accent"
	if _, ok := w.TestRender(nil).FindByID(pickerID); ok {
		t.Fatal("default accent was visible before installation")
	}
	if err := w.TestClick("save-accents"); err != nil {
		t.Fatal(err)
	}
	layout := waitForAccentContinue(t, w)
	if complete || w.CloseRequested() || !completeAccentPack(filepath.Join(accentsDir, pack.name), pack.accents) {
		t.Fatal("Save should install the pack and keep the wizard open")
	}
	// Installing recordings alone must leave first-run settings unwritten.
	if _, err := os.Stat(filepath.Join(dir, config.SettingsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings file changed before Continue: %v", err)
	}
	picker, ok := layout.FindByID(pickerID)
	button, _ := layout.FindByID("continue-accents")
	if !ok || picker.Parent != button.Parent || picker.Shape.X+picker.Shape.Width >= button.Shape.X || button.Shape.Color != green {
		t.Fatal("the footer needs a default accent picker to the left of a green Continue button")
	}
	if err := w.TestClick(pickerID); err != nil {
		t.Fatal(err)
	}
	dropdown, ok := w.TestRender(nil).FindByID(pickerID + ":dropdown")
	if !ok || dropdown.Shape.Y >= picker.Shape.Y {
		t.Fatal("footer dropdown must open upward")
	}
	// Read the rendered dropdown labels to check what the user can actually pick.
	var choices []string
	for _, cmd := range w.Renderers() {
		box := dropdown.Shape
		if cmd.Kind == gui.RenderText && cmd.Text != "✓" && cmd.X >= box.X && cmd.X < box.X+box.Width && cmd.Y >= box.Y && cmd.Y < box.Y+box.Height {
			choices = append(choices, cmd.Text)
		}
	}
	want := slices.Clone(pack.accents)
	slices.Sort(want)
	if !slices.Equal(choices, want) {
		t.Fatalf("default accent choices = %v, want %v", choices, want)
	}
	for _, key := range []gui.KeyCode{gui.KeyDown, gui.KeyEnter} {
		if err := w.TestKey(pickerID, key, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.TestClick("continue-accents"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !complete && time.Now().Before(deadline); {
		w.FrameFn()
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(filepath.Join(dir, config.SettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !complete || !w.CloseRequested() || !bytes.Contains(data, []byte(`"accent_setup_complete": true`)) {
		t.Fatal("setup did not finish after writing the completed setting")
	}
	if !bytes.Contains(data, []byte(`"default_accent": "Australian"`)) {
		t.Fatal("Continue did not save the selected default accent")
	}
	if !completeAccentPack(filepath.Join(accentsDir, pack.name), pack.accents) {
		t.Fatal("no recordings alongside the archive")
	}
	otherDir := filepath.Join(t.TempDir(), "Accents")
	if err := installAccentPacks(t.Context(), accentsDir, otherDir, []string{pack.name}); err != nil {
		t.Fatal(err)
	}
	if !completeAccentPack(filepath.Join(otherDir, pack.name), pack.accents) {
		t.Fatal("a separate data directory skipped extraction")
	}
}

func TestIncompleteAccentDirectoryIsPreservedAndReported(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	recordings := t.TempDir()
	pack := packs[0]
	target := filepath.Join(recordings, pack.name)
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	// Leave a file in the incomplete install to detect any destructive retry.
	path := filepath.Join(target, "keep.txt")
	if err := os.WriteFile(path, []byte("existing recording data"), 0600); err != nil {
		t.Fatal(err)
	}
	err := installAccentPacks(t.Context(), "Accents", recordings, []string{pack.name})
	if err == nil || !strings.Contains(err.Error(), "incomplete recordings") {
		t.Fatalf("install error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "existing recording data" {
		t.Fatalf("existing file = %q, %v", data, err)
	}
}

func TestRestoreAccentSetupFromExtractedPacks(t *testing.T) {
	recordings := t.TempDir()
	pack := packs[5]
	// Use the old flat extraction layout to exercise startup recovery.
	greeting := filepath.Join(recordings, "Extracted", "nigerian", "goahead")
	if err := os.MkdirAll(greeting, 0700); err != nil {
		t.Fatal(err)
	}
	prefs := config.Defaults()
	if restoreAccentSetup(recordings, prefs) {
		t.Fatal("empty accent folder skipped setup")
	}
	for _, voice := range []string{"female", "male"} {
		if err := os.WriteFile(filepath.Join(greeting, voice+"11.ai.mp3"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if !restoreAccentSetup(recordings, prefs) || !config.Get[bool](prefs, "accent_setup_complete") {
		t.Fatal("complete extracted pack did not restore setup")
	}
	if got := config.Get[[]string](prefs, "installed_packs"); !slices.Equal(got, []string{pack.name}) {
		t.Fatalf("restored packs = %v, want %s", got, pack.name)
	}
	if got := config.Get[string](prefs, "default_accent"); got != "Nigerian" {
		t.Fatalf("restored default accent = %q, want Nigerian", got)
	}
	prefs = config.Defaults()
	prefs.Put("installed_packs", []string{packs[0].name})
	if restoreAccentSetup(recordings, prefs) {
		t.Fatal("an unselected pack replaced the saved selection")
	}
}

func TestAccentSetupCheckmarkWhileSaving(t *testing.T) {
	pack := packs[0]
	recordings := t.TempDir()
	if err := installAccentPacks(t.Context(), filepath.Join("..", "..", "Accents"), recordings, []string{pack.name}); err != nil {
		t.Fatal(err)
	}
	// Reuse a complete pack so the test can inspect the locked selection without extracting again.
	view := accentSetupView(t.Context(), t.TempDir(), recordings, config.Defaults(), func([]string, string, func(error)) {}, nil)
	w := gui.NewTestWindow(t, gui.WindowCfg{Width: 720, Height: 600})
	w.TestRender(view)
	if err := w.TestClick("save-accents"); err != nil {
		t.Fatal(err)
	}
	if err := w.TestClick("accent-packs:pack-" + pack.name); !errors.Is(err, gui.ErrTestDisabled) {
		t.Fatalf("pack can be changed while saving: %v", err)
	}
	for _, cmd := range w.Renderers() {
		if cmd.Kind == gui.RenderText && cmd.Text == "✓" && cmd.Color == ink {
			return
		}
	}
	t.Fatal("disabled checkmark did not render in the high-contrast color")
}

func TestRegionalCueUsesEnabledPacks(t *testing.T) {
	archives := filepath.Join("..", "..", "Accents")
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "Accents")
	var enabled []string
	for _, pack := range packs {
		enabled = append(enabled, pack.name)
	}
	if err := installAccentPacks(t.Context(), archives, dir, enabled); err != nil {
		t.Fatal(err)
	}
	s := &state{dir: dataDir, crewVoice: "male"}
	for _, pack := range packs {
		for _, name := range pack.accents {
			pcm, rate := s.accentCue(dir, pack.name, name, "greeting", "", "female")
			if len(pcm) == 0 || rate <= 0 {
				t.Errorf("could not decode greeting for %s", name)
			}
		}
	}
	prefs := config.Defaults()
	prefs.Put("installed_packs", []string{packs[0].name})
	request := playback{prefs: prefs, location: &geo.Position{Country: "FR"}, cue: "jetway", voice: "male"}
	american, _, name := s.regionalCue(request)
	// Request the same cue twice to check that the decoder cache reuses the sample buffer.
	again, rate, cachedName := s.regionalCue(request)
	if name != "American" || cachedName != name || len(again) == 0 || len(again)%4 != 0 || &again[0] != &american[0] || rate == 0 {
		t.Fatal("expected cached stereo PCM")
	}
	request.option = "Disconnect jetway"
	disconnected, _, _ := s.regionalCue(request)
	if len(american) == 0 || len(disconnected) == 0 || bytes.Equal(american, disconnected) {
		t.Fatal("expected different American fallback clips for connect and disconnect")
	}
	prefs.Put("force_accent", "British")
	request.option = ""
	british, _, name := s.regionalCue(request)
	if name != "British" || len(british) == 0 || bytes.Equal(american, british) {
		t.Fatal("forced accent did not override the location and default")
	}
	prefs.Put("installed_packs", []string{})
	if pcm, _, name := s.regionalCue(request); len(pcm) != 0 || name != "" {
		t.Fatal("disabled pack was played")
	}
}

func TestFailedAccentExtractionLeavesNoInstalledPack(t *testing.T) {
	archives, recordings := t.TempDir(), t.TempDir()
	name := packs[0].name
	file, err := os.Create(filepath.Join(archives, name+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for _, path := range []string{"american/goahead/female11.ai.mp3", "../outside/female11.ai.mp3"} {
		entry, err := archive.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("incomplete pack")); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(archive.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	if err := installAccentPacks(t.Context(), archives, recordings, []string{name}); err == nil {
		t.Fatal("incomplete pack installed")
	}
	// Check the whole recordings directory so abandoned staging folders fail the test.
	entries, err := os.ReadDir(recordings)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed extraction left files behind: %v, %v", entries, err)
	}
}

func TestMistralTutorialStaysInSettings(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	s := &state{cfg: config.Defaults(), host: gui.NewApp(), ctx: t.Context(), dir: t.TempDir()}
	s.cfg.Put("speech_provider", "mistral")
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.openSettings()
	cfg := <-s.host.PendingOpen()
	cfg.Height = 3200
	w := gui.NewTestWindow(t, cfg)
	w.TestRender(nil)
	if err := w.TestClick("settings-scroll:provider-mistral"); err != nil {
		t.Fatal(err)
	}
	// Check every tutorial step because only the final page should show the key field.
	for i := range mistralSteps {
		layout := w.TestRender(nil)
		if _, ok := layout.FindByID("mistral-key:voxtral-key"); ok {
			t.Fatalf("key field exposed during tutorial step %d", i+1)
		}
		if _, ok := layout.FindByID("mistral-tutorial:mistral-screenshot"); !ok {
			t.Fatalf("missing screenshot for tutorial step %d", i+1)
		}
		if err := w.TestClick("mistral-next"); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := w.TestRender(nil).FindByID("mistral-key:voxtral-key"); !ok {
		t.Fatal("missing key field after the tutorial")
	}
	if err := w.TestType("mistral-key:voxtral-key", "example"); err != nil {
		t.Fatal(err)
	}
	if err := w.TestClick("mistral-back"); err != nil {
		t.Fatal(err)
	}
	if len(s.host.PendingOpen()) != 0 || config.Get[string](s.cfg, "speech_provider") != "mistral" {
		t.Fatal("tutorial opened another window or changed the provider")
	}
	if s.settingUpMistral {
		t.Fatal("leaving setup kept push-to-talk disabled")
	}
}

func TestSpeechCardsExposeRecoveryActions(t *testing.T) {
	s := &state{cfg: config.Defaults(), speechStatus: "Speech unavailable"}
	setup := speechSetup{local: map[string]localModelStatus{
		"parakeet": {ready: true, cached: true},
		"orukeet":  {cached: true},
	}}
	w := gui.NewTestWindow(t, gui.WindowCfg{})
	// A failed load needs a retry while an incomplete download needs a way to clear its files.
	for id, action := range map[string]string{"parakeet": "retry-parakeet", "orukeet": "delete-orukeet"} {
		layout := w.TestRender(func(w *gui.Window) gui.View { return setup.localCard(s, w, id, id) })
		if _, ok := layout.FindByID(action); !ok {
			t.Errorf("missing recovery action %s", action)
		}
	}
}

func TestEnabledMenuReaderCanRecoverIncompleteModel(t *testing.T) {
	s := &state{dir: t.TempDir(), ctx: t.Context(), cfg: config.Defaults(), host: gui.NewApp()}
	s.cfg.Put(config.MenuReadoutSetting, true)
	s.main = gui.NewTestWindow(t, gui.WindowCfg{})
	model, _ := models.Lookup("kokoro")
	// Create the folder without a verified model to exercise the unfinished install state.
	if err := os.MkdirAll(model.Directory(s.dir), 0700); err != nil {
		t.Fatal(err)
	}
	s.openSettings()
	cfg := <-s.host.PendingOpen()
	cfg.Height = 3200
	settings := gui.NewTestWindow(t, cfg)
	layout := settings.TestRender(nil)
	for _, id := range []string{"settings-scroll:delete-kokoro", "settings-scroll:download-kokoro"} {
		if _, ok := layout.FindByID(id); !ok {
			t.Errorf("missing recovery action %s", id)
		}
	}
}

func TestSpeechModelDownloadButtonCancels(t *testing.T) {
	s := &state{cfg: config.Defaults(), ctx: t.Context(), dir: t.TempDir(), saves: make(chan *saveRequest, 1)}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{})
	model, _ := models.Lookup("orukeet")
	// An incomplete bundle makes the downloader return without using the network
	if err := os.MkdirAll(model.Directory(s.dir), 0700); err != nil {
		t.Fatal(err)
	}
	setup := speechSetup{local: map[string]localModelStatus{"orukeet": {cached: true}}}
	w := gui.NewTestWindow(t, gui.WindowCfg{Width: 432, Height: 180})
	w.TestRender(func(w *gui.Window) gui.View { return setup.localCard(s, w, "orukeet", "Enhanced (Orukeet)") })
	hasLabel := func(label string) bool {
		for _, cmd := range w.Renderers() {
			if cmd.Kind == gui.RenderText && cmd.Text == label {
				return true
			}
		}
		return false
	}
	const buttonID = "download-orukeet"
	if !hasLabel("Download") {
		t.Fatal("missing model should offer Download")
	}
	if err := w.TestClick(buttonID); err != nil {
		t.Fatal(err)
	}
	if !hasLabel("Cancel") || hasLabel("Download") {
		t.Fatal("downloading model should replace Download with Cancel")
	}
	if err := w.TestClick(buttonID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for setup.running != "" && time.Now().Before(deadline) {
		s.main.FrameFn()
		time.Sleep(time.Millisecond)
	}
	w.TestRender(nil)
	if !hasLabel("Download") || !hasLabel("Download canceled") || len(s.saves) != 0 {
		t.Fatal("cancel should restore Download and leave the current provider selected")
	}
}

func TestSpeechModelUseAndDeleteShareRow(t *testing.T) {
	s := &state{cfg: config.Defaults(), saves: make(chan *saveRequest, 1)}
	setup := speechSetup{local: map[string]localModelStatus{
		"parakeet": {ready: true, cached: true},
		"orukeet":  {ready: true, cached: true},
	}}
	w := gui.NewTestWindow(t, gui.WindowCfg{Width: 432, Height: 180})
	layout := w.TestRender(func(w *gui.Window) gui.View { return setup.localCard(s, w, "orukeet", "Enhanced (Orukeet)") })
	use, hasUse := layout.FindByID("provider-orukeet")
	del, hasDelete := layout.FindByID("delete-orukeet")
	if !hasUse || !hasDelete || use.Shape.Y != del.Shape.Y || use.Shape.X >= del.Shape.X {
		t.Fatal("installed model should show Use and Delete on one row")
	}
	if err := w.TestClick("provider-orukeet"); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-s.saves:
		if config.Get[string](req.values, "speech_provider") != "orukeet" {
			t.Fatal("Use queued the wrong provider")
		}
		s.finishSave(req, nil)
	default:
		t.Fatal("Use did not queue a provider change")
	}
	layout = w.TestRender(nil)
	// Once the selection is saved, the active model must stop offering Use.
	if _, ok := layout.FindByID("provider-orukeet"); ok {
		t.Fatal("active model still offers Use")
	}
}

func TestMistralSaveFromClosedWindowKeepsNewTutorialActive(t *testing.T) {
	s := &state{cfg: config.Defaults(), settingUpMistral: true, saves: make(chan *saveRequest, 1)}
	setup := speechSetup{step: len(mistralSteps) + 1, hasKey: true}
	w := gui.NewTestWindow(t, gui.WindowCfg{Width: 560, Height: 720})
	w.TestRender(func(w *gui.Window) gui.View { return setup.mistralPage(s, w) })
	if err := w.TestClick("mistral-key:use-saved-key"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	select {
	// Complete the queued save after the window closes to reproduce the stale callback.
	case req := <-s.saves:
		s.finishSave(req, nil)
	default:
		t.Fatal("selecting Mistral did not queue a save")
	}
	if !s.settingUpMistral {
		t.Fatal("an old save closed the active tutorial")
	}
}
