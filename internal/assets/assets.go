// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package assets

import (
	"embed"
	"encoding/json"
)

//go:embed words.json accent_mappings.json app.png
var Files embed.FS

const AccentMappingsFile = "accent_mappings.json"

// Older override files use the accent_used field.
type AccentMapping struct {
	Country string
	Accent  string `json:"accent_used"`
}

func DecodeAccentMappings(data []byte) ([]AccentMapping, error) {
	// Both bundled defaults and local accent overrides use this format.
	var entries []AccentMapping
	err := json.Unmarshal(data, &entries)
	return entries, err
}
