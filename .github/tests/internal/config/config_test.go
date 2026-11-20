// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacySettingsRoundTrip(t *testing.T) {
	// Include an unknown setting and an old preset to exercise migration and preservation
	legacy := []byte(`{"speech_provider":"retired","use_gsx_choice":true,"custom":{"preserve":true},"presets":[{"name":" Ramp   In ","codeword":" Hi  Ramp ","services":["gpu","JETWAY","gpu","skip"]}]}`)
	source, dest, settings := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, SettingsFile), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "accent_mappings.json"), []byte("[{\"country\":\"Canada\",\"accent_used\":\"American\"}]"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(settings, dest, source); err != nil {
		t.Fatal(err)
	}
	v, err := Load(settings)
	if err != nil {
		t.Fatal(err)
	}
	if Get[string](v, "speech_provider") != "parakeet" || v["use_gsx_choice"] != nil || !Get[map[string]bool](v, "custom")["preserve"] {
		t.Fatalf("incorrect migrated values")
	}
	if _, err := os.Stat(filepath.Join(dest, "accent_mappings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, SettingsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("import also placed settings with the runtime data: %v", err)
	}
	p := Get[[]Preset](v, "presets")
	if len(p) != 1 || p[0].Name != "Ramp In" || p[0].Codeword != "hi ramp" || !p[0].Enabled || len(p[0].Services) != 2 || p[0].Services[0] != "GPU" || p[0].Services[1] != "JETWAY" {
		t.Fatalf("presets = %+v", p)
	}
	before, err := os.ReadFile(filepath.Join(source, SettingsFile))
	if err != nil || !bytes.Equal(before, legacy) {
		t.Fatal("source changed")
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
	t.Setenv("LOCALAPPDATA", t.TempDir())
	runtimeDir, err := DataDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	// A previous build's settings must not bypass Accent Setup.
	if err := os.WriteFile(filepath.Join(runtimeDir, SettingsFile), []byte(`{"accent_setup_complete":true}`), 0600); err != nil {
		t.Fatal(err)
	}
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
	v, _, err := decode([]byte(`{"ptt_bind":{"type":"joy","joy_id":0,"joy_name":"Flight Stick","button":3,"desc":"Flight Stick Btn 3"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b := Get[Binding](v, "ptt_bind")
	if b.Type != "joystick" || b.JoyID != nil || b.JoyName != "Flight Stick" || b.JoyButton == nil || *b.JoyButton != 3 {
		t.Fatalf("legacy binding = %+v", b)
	}
}

func TestInvalidImportPreservesDestination(t *testing.T) {
	source, dest := t.TempDir(), t.TempDir()
	for name, value := range map[string]string{
		SettingsFile:           "{}",
		"words.json":           "{}",
		"accent_mappings.json": "{",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Seed the destination vocabulary so the test can detect a partial import
	path := filepath.Join(dest, "words.json")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(dest, dest, source); err == nil {
		t.Fatal("invalid accent mappings accepted")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "original" {
		t.Fatalf("destination was changed: %q, %v", data, err)
	}
	if _, err := Import(source, source, filepath.Join(source, ".")); err == nil {
		t.Fatal("import accepted its own source as destination")
	}
}

func TestImportRejectsMalformedAccentEntries(t *testing.T) {
	source, dest := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, SettingsFile), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	const filename = "accent_mappings.json"
	prior := []byte(`[{"country":"Canada","accent_used":"American"}]`)
	path := filepath.Join(dest, filename)
	if err := os.WriteFile(path, prior, 0600); err != nil {
		t.Fatal(err)
	}
	// Try both the wrong entry shape and the wrong field type
	for _, raw := range []string{`[42]`, `[{"country":"Canada","accent_used":42}]`} {
		if err := os.WriteFile(filepath.Join(source, filename), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Import(dest, dest, source); err == nil {
			t.Errorf("accepted malformed accent entries: %s", raw)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, prior) {
			t.Fatalf("existing overrides changed: %v", err)
		}
	}
}

func TestMigrateDownloadFlags(t *testing.T) {
	// Retired download flags should be removed while the installed accent selection survives
	values, _, err := decode([]byte(`{"first_launch":true,"model_downloaded_parakeet":true,"installed_packs":["custom"]}`))
	if err != nil || values["first_launch"] != nil || values["model_downloaded_parakeet"] != nil {
		t.Fatalf("obsolete settings survived migration: %v", err)
	}
	if packs := Get[[]string](values, "installed_packs"); len(packs) != 1 || packs[0] != "custom" {
		t.Fatal("imported accent pack selection was lost")
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

func TestCredentialFailureRollsBackImport(t *testing.T) {
	source, dest := t.TempDir(), t.TempDir()
	before := []byte(`{"auto_connect_sim":false}`)
	for path, data := range map[string][]byte{
		filepath.Join(source, SettingsFile): []byte(`{"mistral_api_key":"test"}`),
		filepath.Join(source, "words.json"): []byte("{}"),
		filepath.Join(dest, SettingsFile):   before,
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	unavailable := errors.New("vault unavailable")
	// Inject a late vault failure after the destination files have been written
	if _, err := importSettings(dest, dest, source, func(string) error { return unavailable }, nil); !errors.Is(err, unavailable) {
		t.Fatalf("expected vault failure: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, SettingsFile))
	if err != nil || !bytes.Equal(got, before) {
		t.Fatalf("original settings were not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "words.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new vocabulary survived failed import: %v", err)
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

func TestImportWithoutCredentialPreservesUnmigratedDestination(t *testing.T) {
	source, dest, settings := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(settings, SettingsFile)
	// The existing portable file contains the only copy of the key.
	legacy := []byte(`{"mistral_api_key":"old-fixture"}`)
	for name, data := range map[string][]byte{
		path:                                legacy,
		filepath.Join(source, SettingsFile): []byte(`{"auto_connect_sim":false}`),
		filepath.Join(source, "words.json"): []byte("{}"),
	} {
		if err := os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	unavailable := errors.New("vault unavailable")
	if _, err := importSettings(settings, dest, source, nil, func(string) error { return unavailable }); !errors.Is(err, unavailable) {
		t.Errorf("expected vault failure, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, legacy) {
		t.Fatalf("import changed the settings before migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "words.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("import copied vocabulary before migration: %v", err)
	}
}

func TestImportIgnoresSettingsInRuntimeDirectory(t *testing.T) {
	source, data, settings := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, SettingsFile), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, SettingsFile)
	legacy := []byte(`{"mistral_api_key":"old-fixture"}`)
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := importSettings(settings, data, source, nil, func(string) error {
		t.Fatal("import tried to migrate a key from the runtime directory")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, legacy) {
		t.Fatalf("import changed the runtime directory settings file: %v", err)
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
