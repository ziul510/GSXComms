// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"strings"
	"testing"

	"github.com/ziul510/GSXCommsGo/internal/config"
)

func TestFallbackCrewCues(t *testing.T) {
	// Cover both the fixed status cues and every selectable service.
	for _, cue := range append([]string{"connected", "greeting", "error"}, config.Services...) {
		b, err := loadCue(strings.ToLower(cue))
		// A cue needs at least a quarter second of PCM16 and no partial sample.
		if err != nil || len(b) < 12000 || len(b)%2 != 0 {
			t.Errorf("%s: %d bytes, %v", cue, len(b), err)
		}
	}
}
