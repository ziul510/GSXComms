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
		// Keep invalid arguments distinct from a failure to mute or restore audio.
		os.Exit(2)
	}
	var done chan struct{}
	if !*recovery {
		done = make(chan struct{})
		// EOF releases the mute after playback or when the parent exits
		go func() {
			defer close(done)
			if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
				fmt.Fprintln(os.Stderr, "mute watchdog pipe failed")
			}
		}()
	}
	ready := func() error {
		_, err := fmt.Fprintln(os.Stdout, "ready")
		return err
	}
	report := func(err error) { fmt.Fprintln(os.Stderr, err) }
	status := func(line string) { fmt.Fprintln(os.Stderr, line) }
	if err := mute.Run(*dir, done, ready, report, status); err != nil {
		report(err)
		os.Exit(1)
	}
}
