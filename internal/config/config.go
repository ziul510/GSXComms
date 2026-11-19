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

	"github.com/ziul510/GSXCommsGo/internal/assets"
)

//go:embed defaults.json
var defaultJSON []byte

const SettingsFile = "user_settings.json"
const legacySecret = "mistral_api_key"

type Values map[string]json.RawMessage

type Preset struct {
	Name     string   `json:"name"`
	Codeword string   `json:"codeword"`
	Enabled  bool     `json:"enabled"`
	Services []string `json:"services"`
}

type Binding struct {
	Type        string          `json:"type"`
	Value       json.RawMessage `json:"value,omitempty"`
	Description string          `json:"desc"`
	JoyButton   *int            `json:"button,omitempty"`
	JoyID       json.RawMessage `json:"joy_id,omitempty"`
	JoyName     string          `json:"joy_name,omitempty"`
}

var Services = []string{"GPU", "JETWAY", "STAIRS", "CATERING", "REFUELING", "BOARDING", "DEBOARDING", "PUSHBACK", "DEICE", "LAVATORY", "WATER", "CLEANING"}

func DataDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "GSXCommsGo"), nil
}

var defaults = func() Values {
	var values Values
	if err := json.Unmarshal(defaultJSON, &values); err != nil {
		panic(err)
	}
	return values
}()

func Defaults() Values { return defaults.Copy() }

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
	var fallback T
	_ = json.Unmarshal(defaults[key], &fallback)
	return fallback
}

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
	if errors.Is(err, os.ErrNotExist) {
		return Defaults(), nil
	}
	if err != nil {
		return Defaults(), err
	}
	values, _, err := decode(data)
	if err != nil {
		backup := fmt.Sprintf("%s.corrupt.%d", path, time.Now().UnixNano())
		if renameErr := os.Rename(path, backup); renameErr != nil {
			return Defaults(), errors.Join(err, renameErr)
		}
		return Defaults(), fmt.Errorf("settings were unreadable and backed up: %w", err)
	}
	var before Values
	_ = json.Unmarshal(data, &before)
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
		if json.Compact(&x, value) != nil || json.Compact(&y, b[key]) != nil || !bytes.Equal(x.Bytes(), y.Bytes()) {
			return false
		}
	}
	return true
}

func decode(data []byte) (Values, string, error) {
	var saved Values
	if err := json.Unmarshal(data, &saved); err != nil || saved == nil {
		return nil, "", errors.New("expected a JSON settings object")
	}
	key := Get[string](saved, legacySecret)
	values := Defaults()
	for name, raw := range saved {
		if name != legacySecret {
			values[name] = raw
		}
	}
	for _, name := range []string{"use_gsx_choice", "menu_delay", "key_delay", "auto_choice_delay", "menu_hotkey", "require_sim_ptt", "first_launch", "model_downloaded_parakeet", "model_downloaded_orukeet", "model_downloaded_kokoro-82m-am-adam"} {
		delete(values, name)
	}
	if provider := Get[string](values, "speech_provider"); !slices.Contains([]string{"parakeet", "orukeet", "mistral"}, provider) {
		values.Put("speech_provider", "parakeet")
	}
	values.Put("presets", migratePresets(values["presets"]))
	return values, key, nil
}

func Import(dir, source string) (Values, error) {
	return importSettings(dir, source, SaveKey, migrateLegacyKey)
}

func importSettings(dir, source string, storeKey, migrateKey func(string) error) (Values, error) {
	original, err := os.Stat(source)
	if err != nil {
		return nil, err
	}
	destination, err := os.Stat(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if destination != nil && !destination.IsDir() {
		return nil, errors.New("app data path must be a directory")
	}
	if destination != nil && os.SameFile(original, destination) {
		return nil, errors.New("choose a legacy directory separate from the app data directory")
	}
	data, err := os.ReadFile(filepath.Join(source, SettingsFile))
	if err != nil {
		return nil, err
	}
	values, key, err := decode(data)
	if err != nil {
		return nil, err
	}
	type asset struct {
		name string
		data []byte
	}
	var files []asset
	for _, file := range []string{"words.json", assets.AccentMappingsFile} {
		data, err := os.ReadFile(filepath.Join(source, file))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var valid bool
		if file == assets.AccentMappingsFile {
			entries, err := assets.DecodeAccentMappings(data)
			valid = err == nil && entries != nil
		} else {
			var words map[string]any
			valid = json.Unmarshal(data, &words) == nil && words != nil
		}
		if !valid {
			return nil, fmt.Errorf("invalid %s", file)
		}
		files = append(files, asset{file, data})
	}
	settings, err := settingsJSON(values)
	if err != nil {
		return nil, err
	}
	files = append(files, asset{SettingsFile, settings})
	if key == "" {
		if err := preserveSettingsCredential(filepath.Join(dir, SettingsFile), migrateKey); err != nil {
			return nil, err
		}
	}
	var undo []func() error
	rollback := func(err error) (Values, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			err = errors.Join(err, undo[i]())
		}
		return nil, err
	}
	for _, file := range files {
		path := filepath.Join(dir, file.name)
		prior, readErr := os.ReadFile(path)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return rollback(readErr)
		}
		if err := WriteAtomic(path, file.data); err != nil {
			return rollback(err)
		}
		undo = append(undo, func() error {
			if readErr != nil {
				return os.Remove(path)
			}
			return WriteAtomic(path, prior)
		})
	}
	if key != "" {
		if err := storeKey(key); err != nil {
			return rollback(err)
		}
	}
	return values, nil
}

func settingsJSON(values Values) ([]byte, error) {
	copy := values.Copy()
	delete(copy, legacySecret)
	data, err := json.MarshalIndent(copy, "", "    ")
	return append(data, '\n'), err
}

func Save(dir string, values Values) error {
	return save(dir, values, migrateLegacyKey)
}

func save(dir string, values Values, migrateKey func(string) error) error {
	data, err := settingsJSON(values)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, SettingsFile)
	if err := preserveSettingsCredential(path, migrateKey); err != nil {
		return err
	}
	return WriteAtomic(path, data)
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".gsxcomms-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	return Replace(file.Name(), path)
}

func CleanPreset(p Preset) Preset {
	p.Name = strings.Join(strings.Fields(p.Name), " ")
	p.Codeword = strings.ToLower(strings.Join(strings.Fields(p.Codeword), " "))
	services := make([]string, 0, len(p.Services))
	for _, service := range p.Services {
		service = strings.ToUpper(strings.TrimSpace(service))
		if slices.Contains(Services, service) && !slices.Contains(services, service) {
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
		p := Preset{Enabled: true}
		if json.Unmarshal(entry, &p) != nil {
			continue
		}
		p = CleanPreset(p)
		name := strings.ToLower(p.Name)
		if name == "" || p.Codeword == "" || names[name] || codewords[p.Codeword] {
			continue
		}
		names[name], codewords[p.Codeword] = true, true
		result = append(result, p)
	}
	return result
}
