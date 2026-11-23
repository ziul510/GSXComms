// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"image"
	"testing"
)

// Cover window placement inside a work area, across monitors, and in tight spaces
func TestDialogPosition(t *testing.T) {
	primary := workArea{bounds: image.Rect(0, 0, 1200, 800), scale: 100}
	left := workArea{bounds: image.Rect(-900, 100, 0, 800), scale: 100}
	tests := []struct {
		name               string
		dialog, main, want image.Rectangle
		areas              []workArea
	}{
		{
			name:   "preserve the intended position",
			dialog: image.Rect(600, 100, 1000, 700),
			main:   image.Rect(0, 0, 420, 680),
			want:   image.Rect(600, 100, 1000, 700),
			areas:  []workArea{primary},
		},
		{
			name:   "move an overlapping dialog beside the main window",
			dialog: image.Rect(250, 100, 650, 700),
			main:   image.Rect(0, 0, 420, 680),
			want:   image.Rect(420, 100, 820, 700),
			areas:  []workArea{primary},
		},
		{
			name:   "use the space above the main window",
			dialog: image.Rect(250, 500, 950, 750),
			main:   image.Rect(0, 550, 1200, 800),
			want:   image.Rect(250, 300, 950, 550),
			areas:  []workArea{primary},
		},
		{
			name:   "contain a dialog that extends past the work area",
			dialog: image.Rect(950, 500, 1350, 900),
			want:   image.Rect(800, 400, 1200, 800),
			areas:  []workArea{primary},
		},
		{
			name:   "use a second monitor when the main window fills the first",
			dialog: image.Rect(300, 100, 700, 700),
			main:   primary.bounds,
			want:   image.Rect(-400, 100, 0, 700),
			areas:  []workArea{primary, left},
		},
		{
			name:   "preserve logical size on a higher DPI monitor",
			dialog: image.Rect(300, 100, 700, 500),
			main:   primary.bounds,
			want:   image.Rect(1200, 100, 2000, 900),
			areas:  []workArea{primary, {bounds: image.Rect(1200, 0, 3000, 1400), scale: 200}},
		},
		{
			name:   "fit a dialog taller than the work area",
			dialog: image.Rect(250, 100, 650, 950),
			main:   image.Rect(0, 0, 420, 680),
			want:   image.Rect(420, 0, 820, 800),
			areas:  []workArea{primary},
		},
		{
			name:   "minimize overlap when there is no clear fit",
			dialog: image.Rect(350, 150, 850, 650),
			main:   image.Rect(0, 0, 900, 800),
			want:   image.Rect(700, 150, 1200, 650),
			areas:  []workArea{primary},
		},
		{
			name:   "keep the native placement if no work area is available",
			dialog: image.Rect(350, 150, 850, 650),
			want:   image.Rect(350, 150, 850, 650),
		},
	}
	// Each case checks the window's final outer rectangle
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dialogPosition(tt.dialog, tt.main, primary.scale, tt.areas); got != tt.want {
				t.Fatalf("dialogPosition() = %v, want %v", got, tt.want)
			}
		})
	}
}
