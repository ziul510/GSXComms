// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ziul510/GSXCommsGo/internal/mute"
)

func main() {
	dir := flag.String("data-dir", "", "GSXComms settings directory")
	recovery := flag.Bool("restore", false, "restore audio from an interrupted playback")
	flag.Parse()
	// The player and recovery process need the same absolute journal path
	if !filepath.IsAbs(*dir) {
		os.Exit(2)
	}
	var hold func() error
	if !*recovery {
		hold = func() error {
			// Signal the player only once mute setup has succeeded
			if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
				return err
			}
			// A closed pipe signals playback ended or the parent exited
			_, err := io.Copy(io.Discard, os.Stdin)
			return err
		}
	}
	// A nil hold lets the recovery process replay the journal without taking a new mute
	if err := mute.Run(*dir, hold); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
