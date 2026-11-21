// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

func addonManager() string {
	// FSDreamTeam's user install and older machine install use different registry keys
	for _, candidate := range []struct {
		key         registry.Key
		path, value string
	}{
		{registry.CURRENT_USER, `SOFTWARE\FSDreamTeam`, "root"},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\FSDreamTeam\Addon Manager`, "Path"},
	} {
		key, err := registry.OpenKey(candidate.key, candidate.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		value, _, err := key.GetStringValue(candidate.value)
		key.Close()
		if err == nil && filepath.IsAbs(value) {
			return value
		}
	}
	// Fall back to the default install path when neither registry entry gives an absolute path.
	return `C:\Program Files (x86)\Addon Manager`
}

// The console and GUI both need the installed panel's menu file
func Panel() string {
	// Require a full override path so the panel lookup works from any working directory.
	if override := os.Getenv("GSX_PANEL_DIR"); filepath.IsAbs(override) {
		return override
	}
	return filepath.Join(addonManager(), "MSFS", "fsdreamteam-gsx-pro", "html_ui", "InGamePanels", "FSDT_GSX_Panel")
}
