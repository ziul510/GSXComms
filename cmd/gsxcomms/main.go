// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ziul510/GSXCommsGo/internal/app"
	"github.com/ziul510/GSXCommsGo/internal/config"
)

func main() {
	override := flag.String("data-dir", "", "override the settings and runtime data directories")
	importDir := flag.String("import", "", "import settings from a GSXCommsFiles directory")
	flag.Parse()
	dir, settingsDir, err := appDirectories(*override)
	if err != nil {
		fmt.Fprintln(os.Stderr, "GSXComms directories are unavailable:", err)
		os.Exit(1)
	}
	if *importDir != "" {
		if _, err := config.Import(settingsDir, dir, *importDir); err != nil {
			fmt.Fprintln(os.Stderr, "Import failed:", err)
			os.Exit(1)
		}
	}
	app.Run(dir, settingsDir)
}

func appDirectories(override string) (data, settings string, err error) {
	// An explicit override keeps test and developer runs isolated.
	if override != "" {
		dir, err := filepath.Abs(override)
		return dir, dir, err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	data, err = config.DataDir()
	// Use the executable path because shortcuts can change the working directory.
	return data, filepath.Dir(exe), err
}
