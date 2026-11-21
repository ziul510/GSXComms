// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"os"
)

func main() {
	// Stdin carries diagnostics from the GUI and closes when the session ends
	// Open the console directly because stdout can be redirected.
	console, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		os.Exit(1)
	}
	defer console.Close()
	// Stream diagnostic bytes as they arrive so partial lines are visible too.
	_, _ = io.Copy(console, os.Stdin)
}
