// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPortableSettingsNormalizeSavedValues(t *testing.T) {
	// Include an unknown setting and an old preset to exercise migration and preservation
	legacy := []byte(`{"speech_provider":"retired","use_gsx_choice":true,"legacy_accent_dir":"previous-install","custom":{"preserve":true},"presets":[{"name":" Ramp   In ","codeword":" Hi  Ramp ","services":["gpu","JETWAY","gpu","skip"]}]}`)
	settings := t.TempDir()
	if err := os.WriteFile(filepath.Join(settings, SettingsFile), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	v, err := Load(settings)
	if err != nil {
		t.Fatal(err)
	}
	if Get[string](v, "speech_provider") != "parakeet" || v["use_gsx_choice"] != nil || v["legacy_accent_dir"] != nil || !Get[map[string]bool](v, "custom")["preserve"] {
		t.Fatalf("incorrect migrated values")
	}
	p := Get[[]Preset](v, "presets")
	if len(p) != 1 || p[0].Name != "Ramp In" || p[0].Codeword != "hi ramp" || !p[0].Enabled || len(p[0].Services) != 2 || p[0].Services[0] != "GPU" || p[0].Services[1] != "JETWAY" {
		t.Fatalf("presets = %+v", p)
	}
}

func TestPortableSettingsKeepCustomPresetServices(t *testing.T) {
	dir := t.TempDir()
	values := Defaults()
	values.Put("presets", []Preset{{
		Name: "Turnaround", Codeword: "turnaround", Enabled: true,
		Services: []string{"gpu", "Custom:  Cabin Check  ", "JETWAY", "Custom: cabin check", "Custom: "},
	}})
	if err := Save(dir, values); err != nil {
		t.Fatal(err)
	}
	values, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	presets := Get[[]Preset](values, "presets")
	if len(presets) != 1 || !slices.Equal(presets[0].Services, []string{"GPU", "Custom: Cabin Check", "JETWAY"}) {
		t.Fatalf("unexpected presets: %+v", presets)
	}
}

func TestPortableSettingsKeepExistingFile(t *testing.T) {
	dir := t.TempDir()
	values := Defaults()
	values.Put("volume_voice", 0.77)
	if err := Save(dir, values); err != nil {
		t.Fatal(err)
	}
	// If another instance finishes first, the exclusive publish keeps its file.
	path := filepath.Join(dir, SettingsFile)
	if err := writeAtomic(path, defaultJSON, moveNew); !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected exclusive publish to reject an existing file: %v", err)
	}
	if err := InitializeSettings(dir); err != nil {
		t.Fatal(err)
	}
	if values, err := Load(dir); err != nil || Get[float64](values, "volume_voice") != 0.77 {
		t.Fatalf("portable update was overwritten: %v", err)
	}
}

func TestPortableSettingsStartWithDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := InitializeSettings(dir); err != nil {
		t.Fatal(err)
	}
	values, err := Load(dir)
	if err != nil || Get[bool](values, "accent_setup_complete") {
		t.Fatalf("invalid first-run settings: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, SettingsFile)); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyControllerBindingDropsSDLIndex(t *testing.T) {
	v, err := decode([]byte(`{"ptt_bind":{"type":"joy","joy_id":0,"joy_name":"Flight Stick","button":3,"desc":"Flight Stick Btn 3"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b := Get[Binding](v, "ptt_bind")
	if b.Type != "joystick" || b.JoyID != nil || b.JoyName != "Flight Stick" || b.JoyButton == nil || *b.JoyButton != 3 {
		t.Fatalf("legacy binding = %+v", b)
	}
}

func TestMigrateDownloadFlags(t *testing.T) {
	// Retired download flags should be removed while the installed accent selection survives
	values, err := decode([]byte(`{"first_launch":true,"model_downloaded_parakeet":true,"installed_packs":["custom"]}`))
	if err != nil || values["first_launch"] != nil || values["model_downloaded_parakeet"] != nil {
		t.Fatalf("obsolete settings survived migration: %v", err)
	}
	if packs := Get[[]string](values, "installed_packs"); len(packs) != 1 || packs[0] != "custom" {
		t.Fatal("saved accent pack selection was lost")
	}
}

func TestBroaderModeMigratesEitherLegacyOption(t *testing.T) {
	for _, tc := range []struct {
		settings string
		enabled  bool
	}{
		{`{"enable_menu_matching":true,"enable_gsx_menu_text_correction":false}`, true},
		{`{"enable_menu_matching":false,"enable_gsx_menu_text_correction":true}`, true},
		{`{"enable_menu_matching":false,"enable_gsx_menu_text_correction":false}`, false},
		{`{"enable_menu_matching":false}`, false},
	} {
		v, err := decode([]byte(tc.settings))
		if err != nil || v["enable_gsx_menu_text_correction"] != nil || Get[bool](v, "enable_menu_matching") != tc.enabled {
			t.Errorf("migrating %s: expected Broader Mode enabled=%t, got %v (%v)", tc.settings, tc.enabled, v, err)
		}
	}
}

func TestInvalidBindingUsesCleanDefaults(t *testing.T) {
	values := Defaults()
	// Use a partially decodable binding to catch stale fields leaking into the fallback
	values.Put("ptt_bind", map[string]any{"type": 12, "joy_name": "stale"})
	binding := Get[Binding](values, "ptt_bind")
	if binding.Type != "key" || binding.JoyName != "" {
		t.Fatalf("unexpected fallback binding %+v", binding)
	}
	values.Put("auto_connect_sim", nil)
	if !Get[bool](values, "auto_connect_sim") {
		t.Fatal("null did not fall back to the default")
	}
}

func TestSettingsSavePreservesLegacyCredentialUntilMigrated(t *testing.T) {
	legacy := []byte(`{"mistral_api_key":"old-fixture","auto_connect_sim":true}`)
	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFile)
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	values := Defaults()
	values.Put("auto_connect_sim", false)
	unavailable := errors.New("vault unavailable")
	if err := save(dir, values, func(string) error { return unavailable }); !errors.Is(err, unavailable) {
		t.Errorf("expected vault failure, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, legacy) {
		t.Fatalf("save changed the settings before migration: %v", err)
	}
	// Retry with a working vault to check that migration can still read the old key
	migrated := false
	if err := save(dir, values, func(key string) error {
		migrated = key == "old-fixture"
		return nil
	}); err != nil || !migrated {
		t.Fatalf("retry failed to migrate the credential: %v", err)
	}
	saved, err := Load(dir)
	if err != nil || Get[bool](saved, "auto_connect_sim") || saved[legacySecret] != nil {
		t.Fatalf("retry failed to persist the preference: %v", err)
	}
	// After a successful retry the key must only remain in the vault
	got, err = os.ReadFile(path)
	if err != nil || bytes.Contains(got, []byte(legacySecret)) {
		t.Fatalf("legacy credential remained in the settings file: %v", err)
	}
}

func TestImplicitMigrationKeepsExistingVaultCredential(t *testing.T) {
	// Only an empty vault should accept a key from implicit migration
	for _, stored := range []string{"", "new-fixture"} {
		called := false
		err := migrateKeyIfMissing("old-fixture", func() (string, error) {
			return stored, nil
		}, func(key string) error {
			called = true
			if key != "old-fixture" {
				t.Error("migration wrote the wrong credential")
			}
			return nil
		})
		if err != nil || called != (stored == "") {
			t.Fatalf("unexpected vault write = %v, error = %v", called, err)
		}
	}
}

func TestLegacyPTTBackupOmitsCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFile)
	legacy := []byte("{\"ptt_bind\":{\"type\":\"joy\",\"joy_id\":3,\"button\":2,\"joy_name\":\"Stick\"},\"mistral_api_key\":\"fixture-secret\"}")
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := save(dir, Defaults(), func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path + ".ptt-backup")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("fixture-secret")) || !bytes.Contains(data, []byte("\"joy_id\": 3")) {
		t.Fatal("backup lost the SDL binding or retained the secret")
	}
}
