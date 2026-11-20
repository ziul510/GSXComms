// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package assets

import (
	"embed"
	"encoding/json"
)

//go:embed words.json accent_mappings.json app.png cues/*.pcm.gz
var Files embed.FS

// Imports and runtime lookup share the legacy override filename.
const AccentMappingsFile = "accent_mappings.json"

type AccentMapping struct {
	Country string
	Accent  string `json:"accent_used"`
}

func DecodeAccentMappings(data []byte) ([]AccentMapping, error) {
	var entries []AccentMapping
	// Validate imports with the same entry shape used by the accent lookup.
	err := json.Unmarshal(data, &entries)
	return entries, err
}
