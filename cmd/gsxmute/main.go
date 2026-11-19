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
	if !filepath.IsAbs(*dir) {
		os.Exit(2)
	}
	var hold func() error
	if !*recovery {
		hold = func() error {
			if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
				return err
			}
			_, err := io.Copy(io.Discard, os.Stdin)
			return err
		}
	}
	if err := mute.Run(*dir, hold); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
