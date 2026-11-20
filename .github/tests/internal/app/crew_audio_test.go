// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"strings"
	"testing"

	"github.com/ziul510/GSXCommsGo/internal/config"
)

func TestFallbackCrewCues(t *testing.T) {
	for _, cue := range append([]string{"connected", "greeting", "error"}, config.Services...) {
		b, err := loadCue(strings.ToLower(cue))
		if err != nil || len(b) < 12000 || len(b)%2 != 0 {
			t.Errorf("%s: %d bytes, %v", cue, len(b), err)
		}
	}
}
