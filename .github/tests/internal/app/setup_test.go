// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"path/filepath"
	"slices"
	"testing"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/geo"
)

func TestAccentSetupSavesPackSelection(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	s := &state{cfg: config.Defaults(), host: gui.NewApp(), saves: make(chan *saveRequest, 1)}
	s.openAccents()
	w := gui.NewTestWindow(t, <-s.host.PendingOpen())
	w.TestRender(nil)
	click := func(id string) {
		t.Helper()
		if err := w.TestClick(id); err != nil {
			t.Fatal(err)
		}
	}
	click("select-all")
	click("accent-packs:pack-" + packs[0].name)
	click("save-accents")
	saved := config.Get[[]string]((<-s.saves).values, "installed_packs")
	if len(saved) != len(packs)-1 || slices.Contains(saved, packs[0].name) {
		t.Fatalf("unexpected pack selection: %v", saved)
	}
	s.accentOpen = false
	s.openAccents()
	w = gui.NewTestWindow(t, <-s.host.PendingOpen())
	w.TestRender(nil)
	click("deselect-all")
	click("save-accents")
	next := (<-s.saves).values
	if len(config.Get[[]string](next, "installed_packs")) != 0 || !config.Get[bool](next, "accent_setup_complete") {
		t.Fatal("deselect all did not save an empty, completed selection")
	}
}

func TestRegionalCueUsesEnabledPacks(t *testing.T) {
	dir := filepath.Join("..", "..", "Accents")
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
	s := &state{dir: t.TempDir(), accentsDir: dir, crewVoice: "male"}
	request := playback{prefs: prefs, location: &geo.Position{Country: "FR"}, cue: "jetway"}
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
