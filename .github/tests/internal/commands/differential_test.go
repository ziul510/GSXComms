// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"

	"github.com/ziul510/GSXCommsGo/internal/config"
)

func readTest(t *testing.T, path string, out any) {
	t.Helper()
	// The overlay runs this test from internal/commands, with fixtures kept under .github.
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
	// The expected output was captured from the legacy Python command path.
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
	if _, err := Parse([]byte("{} {}")); err == nil {
		t.Fatal("accepted a second vocabulary object")
	}
}

func TestMatchRequiresWholeNumberTokens(t *testing.T) {
	options := []string{"Jetway", "Catering"}
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
