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
)

func TestAccentSetupSavesPackSelection(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	s := &state{ctx: t.Context(), dir: t.TempDir(), accentsDir: "Accents",
		cfg: config.Defaults(), host: gui.NewApp(), saves: make(chan *saveRequest, 1), saved: make(chan struct{})}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	go s.settingsWriter()
	t.Cleanup(func() { close(s.saves); <-s.saved })
	s.openAccents()
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
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			w.FrameFn()
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
		if accentPackInstalled(recordings, pack.name) != slices.Contains(saved, pack.name) {
			t.Errorf("extracted packs do not match settings for %s", pack.name)
		}
	}
	if _, err := os.Stat(filepath.Join(s.dir, config.SettingsFile)); err != nil {
		t.Fatal(err)
	}
	s.openAccents()
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

func TestFirstRunCompletesOnlyAfterCreatingSettings(t *testing.T) {
	dir := t.TempDir()
	accentsDir := filepath.Join(dir, "Accents")
	if err := os.MkdirAll(accentsDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Portable installs extract recordings beside the source archive.
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
	w := gui.NewTestWindow(t, cfg)
	w.TestRender(nil)
	if complete || w.CloseRequested() {
		t.Fatal("setup completed before the user saved")
	}
	if err := w.TestClick("accent-packs:pack-" + pack.name); err != nil {
		t.Fatal(err)
	}
	if err := w.TestClick("save-accents"); err != nil {
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
	if !accentPackInstalled(accentsDir, pack.name) {
		t.Fatal("no recordings alongside the archive")
	}
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
	for _, pack := range packs {
		for _, name := range pack.accents {
			pcm, rate := accentCue(dir, []string{pack.name}, name, "greeting", "", "female")
			if len(pcm) == 0 || rate <= 0 {
				t.Errorf("could not decode greeting for %s", name)
			}
		}
	}
	prefs := config.Defaults()
	prefs.Put("installed_packs", []string{packs[0].name})
	s := &state{dir: dataDir, crewVoice: "male"}
	request := playback{prefs: prefs, location: &geo.Position{Country: "FR"}, cue: "jetway", voice: "male"}
	american, _ := s.regionalCue(request)
	request.option = "Disconnect jetway"
	disconnected, _ := s.regionalCue(request)
	if len(american) == 0 || len(disconnected) == 0 || bytes.Equal(american, disconnected) {
		t.Fatal("expected different American fallback clips for connect and disconnect")
	}
	prefs.Put("force_accent", "British")
	request.option = ""
	british, _ := s.regionalCue(request)
	if len(british) == 0 || bytes.Equal(american, british) {
		t.Fatal("forced accent did not override the location and default")
	}
	prefs.Put("installed_packs", []string{})
	if pcm, _ := s.regionalCue(request); len(pcm) != 0 {
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
	entries, err := os.ReadDir(recordings)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed extraction left files behind: %v, %v", entries, err)
	}
}

func TestMistralTutorialStaysInSettings(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	s := &state{cfg: config.Defaults(), host: gui.NewApp(), ctx: t.Context(), dir: t.TempDir()}
	s.main = gui.NewTestWindow(t, gui.WindowCfg{State: s})
	s.openSettings()
	cfg := <-s.host.PendingOpen()
	cfg.Height = 3200
	w := gui.NewTestWindow(t, cfg)
	w.TestRender(nil)
	if err := w.TestClick("settings-scroll:provider-mistral"); err != nil {
		t.Fatal(err)
	}
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
	if len(s.host.PendingOpen()) != 0 || config.Get[string](s.cfg, "speech_provider") == "mistral" {
		t.Fatal("tutorial opened another window or activated the cloud provider")
	}
	if s.settingUpMistral {
		t.Fatal("leaving setup kept push-to-talk disabled")
	}
}
