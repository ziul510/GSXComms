// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"image"
	"os"
	"testing"
	"time"
	"unsafe"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/go-gui-org/go-gui/gui/backend"
)

// Check that the first layout uses the client size after native placement
func TestLiveDialogPlacementSize(t *testing.T) {
	if os.Getenv("GSXCOMMS_TEST_LIVE_WINDOWS_UI") != "1" {
		t.Skip("enable with GSXCOMMS_TEST_LIVE_WINDOWS_UI=1 on an interactive Windows desktop")
	}
	t.Setenv("GOGUI_DEVICE_SCALE", "1")
	host := gui.NewApp()
	// Let the backend destroy both native windows before leaving the test
	host.ExitMode = gui.ExitOnTrayRemoved
	const title = "GSXComms dialog placement test"
	const requestedHeight = 16000
	var client screenRect
	var got image.Point
	var stopPlacement func()
	var main *gui.Window
	main = gui.NewWindow(gui.WindowCfg{
		Title: "GSXComms", Width: 280, Height: 220,
		OnInit: func(*gui.Window) {
			stopPlacement = installDialogPlacement()
			if stopPlacement == nil {
				t.Error("could not install dialog placement")
				return
			}
			host.OpenWindow(gui.WindowCfg{
				Title: title, Width: 280, Height: requestedHeight,
				OnInit: func(w *gui.Window) {
					hwnd := windowByTitle(title)
					ok, _, _ := user.NewProc("GetClientRect").Call(hwnd, uintptr(unsafe.Pointer(&client)))
					if ok == 0 {
						t.Error("could not read the dialog client size")
					}
					w.SetView(func(w *gui.Window) gui.View {
						// Capture the first layout, before a later resize can repair it
						if got == (image.Point{}) {
							got.X, got.Y = w.WindowSize()
							w.Close()
							main.Close()
						}
						return gui.Label(title, gui.TextStyle{})
					})
				},
			})
		},
	})
	// Close every remaining window even when the dialog fails to open or paint
	timeout := time.AfterFunc(3*time.Second, func() {
		host.Broadcast(func(w *gui.Window) { w.Close() })
	})
	defer timeout.Stop()
	backend.RunApp(host, main)
	if stopPlacement != nil {
		stopPlacement()
	}
	want := client.rectangle().Size()
	if want.Y <= 0 || want.Y >= requestedHeight {
		t.Fatalf("dialog did not shrink to the work area: client size %v", want)
	}
	if got != want {
		t.Fatalf("first dialog layout used %v, want native client size %v", got, want)
	}
}

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
