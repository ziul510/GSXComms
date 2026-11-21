// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed defaults.json
var defaultJSON []byte

const SettingsFile = "user_settings.json"
const legacySecret = "mistral_api_key"

// Keep saved values in JSON form so settings can preserve unrecognized keys
type Values map[string]json.RawMessage

type Preset struct {
	Name     string   `json:"name"`
	Codeword string   `json:"codeword"`
	Enabled  bool     `json:"enabled"`
	Services []string `json:"services"`
}

// Legacy bindings can use strings for keys or device names Current bindings carry native IDs
type Binding struct {
	Type        string          `json:"type"`
	Value       json.RawMessage `json:"value,omitempty"`
	Description string          `json:"desc"`
	JoyButton   *int            `json:"button,omitempty"`
	JoyID       json.RawMessage `json:"joy_id,omitempty"`
	JoyName     string          `json:"joy_name,omitempty"`
}

var Services = []string{"GPU", "JETWAY", "STAIRS", "CATERING", "REFUELING", "BOARDING", "DEBOARDING", "PUSHBACK", "DEICE", "LAVATORY", "WATER", "CLEANING"}

// Keep custom steps in the same ordered list as built-in service IDs
const CustomServicePrefix = "Custom:"

// InitializeSettings creates the portable file from defaults before Accent Setup opens.
func InitializeSettings(dir string) error {
	path := filepath.Join(dir, SettingsFile)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := settingsJSON(Defaults())
	if err != nil {
		return err
	}
	// Publish only if another instance has not already created the file.
	err = writeAtomic(path, data, moveNew)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not create settings file %s: %w", path, err)
	}
	return nil
}

// Load the embedded defaults once Callers edit copies
var defaults = func() Values {
	var values Values
	if err := json.Unmarshal(defaultJSON, &values); err != nil {
		panic(err)
	}
	return values
}()

// Give each window its own copy of the defaults.
func Defaults() Values { return defaults.Copy() }

// Clone the raw JSON bytes too so a background job gets an independent snapshot
func (v Values) Copy() Values {
	result := make(Values, len(v))
	for key, raw := range v {
		result[key] = bytes.Clone(raw)
	}
	return result
}

func Get[T any](v Values, key string) T {
	var value T
	raw := v[key]
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Unmarshal(raw, &value) == nil {
		return value
	}
	// A failed decode can leave partial state in value Start fresh for the fallback
	var fallback T
	_ = json.Unmarshal(defaults[key], &fallback)
	return fallback
}

// Runtime edits must keep the Voxtral key out of the settings map
func (v Values) Put(key string, value any) {
	if key == legacySecret {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	v[key] = raw
}

func Load(dir string) (Values, error) {
	path := filepath.Join(dir, SettingsFile)
	data, err := os.ReadFile(path)
	// A missing file is a normal first launch
	if errors.Is(err, os.ErrNotExist) {
		return Defaults(), nil
	}
	if err != nil {
		return Defaults(), err
	}
	values, err := decode(data)
	if err != nil {
		// Keep the bad file around in case its settings can be recovered
		backup := fmt.Sprintf("%s.corrupt.%d", path, time.Now().UnixNano())
		if renameErr := os.Rename(path, backup); renameErr != nil {
			return Defaults(), errors.Join(err, renameErr)
		}
		return Defaults(), fmt.Errorf("settings were unreadable and backed up: %w", err)
	}
	var before Values
	_ = json.Unmarshal(data, &before)
	// Persist migration only when the effective contents changed
	if !equalValues(before, values) {
		return values, Save(dir, values)
	}
	return values, nil
}

func equalValues(a, b Values) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		var x, y bytes.Buffer
		// Ignore JSON spacing so opening the app doesn't rewrite settings needlessly
		if json.Compact(&x, value) != nil || json.Compact(&y, b[key]) != nil || !bytes.Equal(x.Bytes(), y.Bytes()) {
			return false
		}
	}
	return true
}

func decode(data []byte) (Values, error) {
	var saved Values
	if err := json.Unmarshal(data, &saved); err != nil || saved == nil {
		return nil, errors.New("expected a JSON settings object")
	}
	// Older settings can omit new keys and still carry user-defined values
	values := Defaults()
	for name, raw := range saved {
		if name != legacySecret {
			values[name] = raw
		}
	}
	// Keep Broader Mode enabled if either previous menu option was enabled
	if Get[bool](saved, "enable_gsx_menu_text_correction") {
		values.Put("enable_menu_matching", true)
	}
	// Retired options and download flags have no meaning in the current app
	for _, name := range []string{
		"use_gsx_choice", "menu_delay", "key_delay", "auto_choice_delay", "menu_hotkey", "require_sim_ptt",
		"first_launch", "model_downloaded_parakeet", "model_downloaded_orukeet", "model_downloaded_kokoro-82m-am-adam", "legacy_accent_dir",
		"enable_gsx_menu_text_correction",
	} {
		delete(values, name)
	}
	// A retired provider falls back to the bundled default selection
	if provider := Get[string](values, "speech_provider"); !slices.Contains([]string{"parakeet", "orukeet", "mistral"}, provider) {
		values.Put("speech_provider", "parakeet")
	}
	values.Put("presets", migratePresets(values["presets"]))
	if binding := Get[Binding](values, "ptt_bind"); binding.Type == "joy" {
		binding.Type = "joystick"
		// SDL indexes identify a device only within the legacy process.
		binding.JoyID = nil
		values.Put("ptt_bind", binding)
	}
	return values, nil
}

// Strip the credential again at the serialization boundary
func settingsJSON(values Values) ([]byte, error) {
	copy := values.Copy()
	delete(copy, legacySecret)
	data, err := json.MarshalIndent(copy, "", "    ")
	return append(data, '\n'), err
}

// Normal saves migrate an old credential before replacing the settings file
func Save(dir string, values Values) error {
	return save(dir, values, migrateLegacyKey)
}

func save(dir string, values Values, migrateKey func(string) error) error {
	data, err := settingsJSON(values)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, SettingsFile)
	// The old JSON may be the only copy of the Voxtral key
	if err := preserveSettingsCredential(path, migrateKey); err != nil {
		return err
	}
	if err := backupLegacyBinding(path); err != nil {
		return err
	}
	return WriteAtomic(path, data)
}

// Read the destination before overwriting it in case it still holds an unmigrated key
func backupLegacyBinding(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved Values
	if json.Unmarshal(data, &saved) != nil {
		return nil
	}
	binding := Get[Binding](saved, "ptt_bind")
	if _, python := saved["first_launch"]; !python && binding.Type != "joy" &&
		!(binding.Type == "joystick" && len(binding.JoyID) == 0) {
		return nil
	}
	// The backup preserves the old binding while the credential stays in the vault
	if _, exists := saved[legacySecret]; exists {
		data, err = settingsJSON(saved)
		if err != nil {
			return err
		}
	}
	err = writeAtomic(path+".ptt-backup", data, moveNew)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not back up legacy PTT settings: %w", err)
	}
	return nil
}

func preserveSettingsCredential(path string, migrateKey func(string) error) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved Values
	if json.Unmarshal(data, &saved) == nil {
		if key := Get[string](saved, legacySecret); key != "" {
			if err := migrateKey(key); err != nil {
				return fmt.Errorf("could not migrate Voxtral credential: %w", err)
			}
		}
	}
	return nil
}

func WriteAtomic(path string, data []byte) error {
	return writeAtomic(path, data, Replace)
}

func writeAtomic(path string, data []byte, publish func(string, string) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Stage beside the destination so replacement stays on the same volume
	file, err := os.CreateTemp(dir, ".gsxcomms-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	// Flush and close the temporary file before replacing the destination
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	return publish(file.Name(), path)
}

// Normalize labels and codewords while keeping the user's service order
func CleanPreset(p Preset) Preset {
	p.Name = strings.Join(strings.Fields(p.Name), " ")
	p.Codeword = strings.ToLower(strings.Join(strings.Fields(p.Codeword), " "))
	services := make([]string, 0, len(p.Services))
	for _, service := range p.Services {
		service = strings.TrimSpace(service)
		if text, custom := strings.CutPrefix(service, CustomServicePrefix); custom {
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			service = CustomServicePrefix + " " + text
		} else {
			service = strings.ToUpper(service)
			if !slices.Contains(Services, service) {
				continue
			}
		}
		// Preserve the first occurrence of case-insensitive matches
		if !slices.ContainsFunc(services, func(other string) bool { return strings.EqualFold(other, service) }) {
			services = append(services, service)
		}
	}
	p.Services = services
	return p
}

func ValidatePreset(p Preset, entries []Preset, index int) (Preset, error) {
	p = CleanPreset(p)
	switch {
	case p.Name == "":
		return p, errors.New("preset name is required")
	case utf8.RuneCountInString(p.Codeword) < 2:
		return p, errors.New("codeword must be at least 2 characters")
	case len(p.Services) == 0:
		return p, errors.New("select at least one service")
	}
	for i, other := range entries {
		// An edited preset can keep its own name and codeword
		if i == index {
			continue
		}
		if strings.EqualFold(other.Name, p.Name) {
			return p, errors.New("a preset with this name already exists")
		}
		if other.Codeword == p.Codeword {
			return p, errors.New("this codeword is already used by another preset")
		}
	}
	return p, nil
}

func migratePresets(raw json.RawMessage) []Preset {
	var entries []json.RawMessage
	_ = json.Unmarshal(raw, &entries)
	result := make([]Preset, 0, len(entries))
	names, codewords := map[string]bool{}, map[string]bool{}
	for _, entry := range entries {
		// Omitted legacy enabled flags should leave those presets active
		p := Preset{Enabled: true}
		if json.Unmarshal(entry, &p) != nil {
			continue
		}
		p = CleanPreset(p)
		name := strings.ToLower(p.Name)
		// Keep the first valid entry for each name and trigger in saved presets.
		if name == "" || p.Codeword == "" || names[name] || codewords[p.Codeword] {
			continue
		}
		names[name], codewords[p.Codeword] = true, true
		result = append(result, p)
	}
	return result
}
