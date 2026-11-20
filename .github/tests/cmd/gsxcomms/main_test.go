// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsDirectoryIgnoresWorkingDirectory(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	_, settingsDir, err := appDirectories("")
	if err != nil {
		t.Fatal(err)
	}
	if settingsDir != filepath.Dir(exe) {
		t.Fatalf("settings directory is %q for executable %q", settingsDir, exe)
	}
}
