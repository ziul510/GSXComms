// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package assets

import (
	"embed"
	"encoding/json"
)

//go:embed words.json accent_mappings.json app.png cues/*.pcm.gz
var Files embed.FS

const AccentMappingsFile = "accent_mappings.json"

type AccentMapping struct {
	Country string
	Accent  string `json:"accent_used"`
}

func DecodeAccentMappings(data []byte) ([]AccentMapping, error) {
	var entries []AccentMapping
	err := json.Unmarshal(data, &entries)
	return entries, err
}
