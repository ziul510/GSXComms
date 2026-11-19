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
	dir, _ := config.DataDir()
	flag.StringVar(&dir, "data-dir", dir, "directory for GSXComms settings")
	importDir := flag.String("import", "", "import settings from a GSXCommsFiles directory")
	flag.Parse()
	if dir == "" {
		fmt.Fprintln(os.Stderr, "GSXComms data directory is unavailable")
		os.Exit(1)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Invalid GSXComms data directory:", err)
		os.Exit(1)
	}
	if *importDir != "" {
		if _, err := config.Import(dir, *importDir); err != nil {
			fmt.Fprintln(os.Stderr, "Import failed:", err)
			os.Exit(1)
		}
	}
	app.Run(dir)
}
