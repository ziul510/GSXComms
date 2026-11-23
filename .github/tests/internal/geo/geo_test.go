// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package geo

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestAirportLocations(t *testing.T) {
	for _, test := range []struct {
		lat, lon   float64
		cc, accent string
	}{
		// Edinburgh covers a regional override The other airports cover country defaults
		{55.949, -3.364, "GB", "Scottish"}, {48.3538, 11.7861, "DE", "German"}, {35.5494, 139.7798, "JP", "Japanese"},
	} {
		p, err := Nearest(test.lat, test.lon)
		// A regional accent must precede broader country candidates.
		candidates := AccentCandidates(p, nil)
		first := slices.IndexFunc(candidates, func(name string) bool { return name != "" })
		if err != nil || p.Country != test.cc || first < 0 || candidates[first] != test.accent {
			t.Errorf("lookup = %#v, %v", p, err)
		}
	}
}

func TestLocalOverrideByCountryNameAndCode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "accent_mappings.json"), []byte(`[{"country":"Germany","accent_used":"French"},{"country":"CA","accent_used":"British"},{"country":"United Kingdom","accent_used":"Irish"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	overrides := Overrides(dir)
	for _, sample := range []struct {
		pos  Position
		want string
	}{
		{Position{Country: "DE"}, "French"},
		{Position{Country: "CA"}, "British"},
		// A local country override takes precedence over the stock regional accent
		{Position{Country: "GB", Region: "Scotland"}, "Irish"},
	} {
		candidates := AccentCandidates(sample.pos, overrides)
		// Missing mappings leave blank entries ahead of the first usable accent.
		first := slices.IndexFunc(candidates, func(name string) bool { return name != "" })
		if first < 0 || candidates[first] != sample.want {
			t.Errorf("%v: candidates %q, want %q first", sample.pos, candidates, sample.want)
		}
	}
}
