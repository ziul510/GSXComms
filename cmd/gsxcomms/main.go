// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ziul510/GSXCommsGo/internal/app"
)

func main() {
	override := flag.String("data-dir", "", "override the settings and runtime data directory")
	flag.Parse()
	dir, err := appDirectory(*override)
	if err != nil {
		fmt.Fprintln(os.Stderr, "GSXComms data directory is unavailable:", err)
		os.Exit(1)
	}
	// Pass the resolved directory into the app so settings and runtime data share a base.
	app.Run(dir)
}

func appDirectory(override string) (string, error) {
	// An explicit override keeps test and developer runs isolated
	if override != "" {
		// Resolve relative overrides before the app starts opening files under that directory.
		return filepath.Abs(override)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Use the executable path because shortcuts can change the working directory
	return filepath.Dir(exe), nil
}
