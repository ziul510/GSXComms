// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package geo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAirportLocations(t *testing.T) {
	for _, test := range []struct {
		lat, lon   float64
		cc, accent string
	}{
		{55.949, -3.364, "GB", "Scottish"}, {48.3538, 11.7861, "DE", "German"}, {35.5494, 139.7798, "JP", "Japanese"},
	} {
		p, err := Nearest(test.lat, test.lon)
		if err != nil || p.Country != test.cc || Accent(p, Overrides(t.TempDir()), "American", "") != test.accent {
			t.Errorf("lookup = %#v, %v", p, err)
		}
	}
}

func TestImportedCountryNameAndCode(t *testing.T) {
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
		{Position{Country: "GB", Region: "Scotland"}, "Irish"},
	} {
		if got := Accent(sample.pos, overrides, "American", ""); got != sample.want {
			t.Errorf("%v: got %q, want %q", sample.pos, got, sample.want)
		}
	}
}
