// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDataDirectoryUsesExecutableLocation(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	dir, err := appDirectory("")
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Dir(exe) {
		t.Fatalf("data directory is %q for executable %q", dir, exe)
	}
}
