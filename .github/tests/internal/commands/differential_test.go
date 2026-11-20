// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ziul510/GSXCommsGo/internal/config"
)

func TestCorrectLocalTranscript(t *testing.T) {
	for input, want := range map[string]string{
		"Cock pitch a ground.":            "cockpit to ground.",
		"clock pit to ground":             "cockpit to ground",
		"Clock picture ground.":           "cockpit to ground.",
		"Cock pit two ground.":            "cockpit to ground.",
		"Clock pit a ground.":             "cockpit to ground.",
		"Clock pit to ground, X-ray GPU.": "cockpit to ground, request GPU.",
		"Please x ray pushback":           "Please request pushback",
		"Ground, re quest ground powder":  "Ground, request ground power",
		"Connect the gee pee you":         "Connect the GPU",
		"Connect the G P U":               "Connect the GPU",
		"Connect the jet way":             "Connect the jetway",
		"Connect the jet breach":          "Connect the jet bridge",
		"Bring the air stainers":          "Bring the air stairs",
		"Bring the air stares":            "Bring the air stairs",
		"Request the fuel track":          "Request the fuel truck",
		"Request the fool truck":          "Request the fuel truck",
		"Request de-sing":                 "Request deicing",
		"Start de boarding":               "Start deboarding",
		"Request laboratory service":      "Request lavatory service",
		"Request the laboratory truck":    "Request the lavatory truck",
		"X-ray.":                          "request.",
		"request x-ray juliett alfa":      "request x-ray juliett alfa",
		"Gate X-ray four":                 "Gate X-ray four",
		"clock pit to grounding":          "clock pit to grounding",
		"x-raymond":                       "x-raymond",
		"Check the fuel tracking":         "Check the fuel tracking",
		"Gate D boarding":                 "Gate D boarding",
		"Check the laboratory results":    "Check the laboratory results",
	} {
		if got := CorrectLocalTranscript(input); got != want {
			t.Errorf("CorrectLocalTranscript(%q) = %q, want %q", input, got, want)
		}
	}
}

func readTest(t *testing.T, path string, out any) {
	t.Helper()
	// The overlay runs this test from internal/commands, with fixtures kept under .github
	data, err := os.ReadFile("../../.github/tests/testdata/" + path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func TestReferenceCorpus(t *testing.T) {
	var input struct {
		Commands []string
		Presets  []config.Preset
		Menus    []struct {
			Text    string
			Options []string
		}
	}
	var expected struct {
		Hotwords []string
		Commands []struct {
			Text  string
			Route Action
		}
		Menus []struct {
			Index, Points int
			Correction    string
			Similarity    float64
		}
	}
	readTest(t, "corpus.json", &input)
	// The expected output was captured from the legacy Python command path
	readTest(t, "reference.json", &expected)
	vocab, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(vocab.Hotwords(), expected.Hotwords) {
		t.Fatal("decoder vocabulary differs from the Python reference")
	}
	for i, raw := range input.Commands {
		normal := vocab.Normalize(raw)
		route := vocab.Route(normal, raw, input.Presets, true)
		if want := expected.Commands[i]; normal != want.Text || !reflect.DeepEqual(route, want.Route) {
			t.Errorf("%q: got %q %+v, want %q %+v", raw, normal, route, want.Text, want.Route)
		}
	}
	for i, entry := range input.Menus {
		index, score := Match(entry.Text, entry.Options)
		corrected, similarity := CorrectMenu(entry.Text, entry.Options)
		if want := expected.Menus[i]; index != want.Index || score != want.Points || corrected != want.Correction || math.Abs(similarity-want.Similarity) > 1e-9 {
			t.Errorf("%q: got index=%d score=%d correction=%q similarity=%v, want %+v", entry.Text, index, score, corrected, similarity, want)
		}
	}
}

func TestParseRejectsTrailingVocabulary(t *testing.T) {
	// A decoder that stops after the first object would otherwise accept this input
	if _, err := Parse([]byte("{} {}")); err == nil {
		t.Fatal("accepted a second vocabulary object")
	}
}

func TestMatchRequiresWholeNumberTokens(t *testing.T) {
	options := []string{"Jetway", "Catering"}
	// Keep multi-digit numbers from matching a single-digit menu slot
	for text, want := range map[string]int{
		"option 10":              -1,
		"number 21":              -1,
		"please choose option 1": 0,
		"option 21 option 2":     1,
	} {
		if got, _ := Match(text, options); got != want {
			t.Errorf("%q: matched item %d, want %d", text, got, want)
		}
	}
}

func TestLegacyDirectMenuNumbers(t *testing.T) {
	for _, text := range []string{"3.", " three? ", "THREE", "3"} {
		if index, ok := Number(text); !ok || index != 2 {
			t.Errorf("expected menu slot three for %q, got %d, %v", text, index, ok)
		}
	}
	for _, text := range []string{"option 3", "third", "10", "3rd"} {
		if _, ok := Number(text); ok {
			t.Errorf("%q was accepted as a bare menu number", text)
		}
	}
}

func TestVocabularyKeepsLastGoodOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "words.json")
	var loader Loader
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("\ufeff" + "{\"CUSTOM\":{\"keywords\":\"ramp\",\"bias_variants\":[\"ramps\"]}}")
	if vocab, warning := loader.Load(dir); warning != "" || vocab.Normalize("ramps") != "ramp" {
		t.Fatalf("valid BOM override warning: %s", warning)
	}
	write("{\"CUSTOM\":{\"keywords\":[false]}}")
	if vocab, warning := loader.Load(dir); !strings.Contains(warning, "expected strings") || vocab.Normalize("ramps") != "ramp" {
		t.Fatalf("failed to keep last good override: %s", warning)
	}
	if _, warning := loader.Load(dir); warning != "" {
		t.Fatal("repeated the parse warning without a change")
	}
	write("{\"CUSTOM\":{\"keywords\":[\"ramp two\"],\"bias_variants\":\"ramps\"}}")
	if vocab, warning := loader.Load(dir); warning != "" || vocab.Normalize("ramps") != "ramp two" {
		t.Fatalf("failed to reload corrected override: %s", warning)
	}
}
