//go:build windows && cgo

// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"testing"
)

func TestSpeechSpans(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames string
		want   []sampleSpan
	}{
		{"ignore short bursts", "..S.....SSSS.....", nil},
		{"keep weak continuation", ".........Swwww.....", []sampleSpan{{4, 19}}},
		{"keep a short pause", "SSSSS......SSSSS.....", []sampleSpan{{0, 21}}},
		{"remove a long pause", "SSSSS.................SSSSS", []sampleSpan{{0, 10}, {17, 32}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var spans speechSpans
			// Use one character per frame to test span boundaries without running the model.
			for i, frame := range tc.frames {
				spans.observe(i*vadWindow, map[rune]float32{'S': 0.8, 'w': 0.4, '.': 0.1}[frame])
			}
			spans.finish(len(tc.frames) * vadWindow)
			// Compare offsets in frames so the expected spans match the fixture strings.
			for i := range spans.kept {
				spans.kept[i].start /= vadWindow
				spans.kept[i].end /= vadWindow
			}
			if !slices.Equal(spans.kept, tc.want) {
				t.Fatalf("kept spans = %v, want %v", spans.kept, tc.want)
			}
		})
	}
}
