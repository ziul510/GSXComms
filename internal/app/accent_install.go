// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var accentClipPath = regexp.MustCompile(`^[a-z]+/[a-z]+/(female|male)(connect|disconnect)?11\.ai\.mp3$`)

func completeAccentPack(dir string, accents []string) bool {
	for _, accent := range accents {
		for _, voice := range []string{"female", "male"} {
			info, err := os.Stat(filepath.Join(dir, strings.ToLower(accent), "goahead", voice+"11.ai.mp3"))
			// A pack counts as installed only when both greeting voices exist for each accent.
			if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
				return false
			}
		}
	}
	return true
}

// Legacy archives can be unpacked by pack or flattened under Extracted
func legacyAccentPack(root, name string, accents []string) bool {
	for _, relative := range []string{name, "Extracted", ""} {
		if completeAccentPack(filepath.Join(root, relative), accents) {
			return true
		}
	}
	return false
}

func installAccentPacks(ctx context.Context, archives, recordings string, enabled []string) error {
	for _, pack := range packs {
		// Reuse complete recordings from older installs before trying to open an archive.
		if !slices.Contains(enabled, pack.name) || legacyAccentPack(recordings, pack.name, pack.accents) {
			continue
		}
		if err := installAccentPack(ctx, archives, recordings, pack.name, pack.accents); err != nil {
			return fmt.Errorf("Could not install %s: %w", pack.name, err)
		}
	}
	return nil
}

func installAccentPack(ctx context.Context, archives, recordings, name string, accents []string) error {
	archive, err := zip.OpenReader(filepath.Join(archives, name+".zip"))
	if err != nil {
		return err
	}
	defer archive.Close()

	if err := os.MkdirAll(recordings, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(recordings, ".pack-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	installed := make(map[string]bool)
	for _, entry := range archive.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Normalize clip names before filtering so they match the names playback looks up.
		path := strings.ToLower(entry.Name)
		if !accentClipPath.MatchString(path) {
			continue
		}
		accentName, _, _ := strings.Cut(path, "/")
		if !slices.ContainsFunc(accents, func(accent string) bool { return strings.EqualFold(accent, accentName) }) {
			continue
		}
		if installed[path] || len(installed) >= 256 || !entry.Mode().IsRegular() || entry.UncompressedSize64 > maxAccentClipBytes {
			return fmt.Errorf("invalid recording %q", entry.Name)
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		// Check the actual decompressed size because the ZIP header can be wrong.
		data, readErr := io.ReadAll(io.LimitReader(source, maxAccentClipBytes+1))
		source.Close()
		if readErr != nil {
			return readErr
		}
		if len(data) == 0 || len(data) > maxAccentClipBytes {
			return fmt.Errorf("invalid recording size for %q", entry.Name)
		}
		target := filepath.Join(stage, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0600); err != nil {
			return err
		}
		installed[path] = true
	}
	for _, accent := range accents {
		greeting := strings.ToLower(accent) + "/goahead/"
		if !installed[greeting+"female11.ai.mp3"] || !installed[greeting+"male11.ai.mp3"] {
			return fmt.Errorf("missing greeting recordings for %s", accent)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Only a complete pack becomes visible to playback and concurrent app instances
	target := filepath.Join(recordings, name)
	if err = os.Rename(stage, target); err != nil {
		// A concurrent installer can finish first, so accept its pack if it passes the same check.
		if completeAccentPack(target, accents) {
			return nil
		}
		if _, statErr := os.Stat(target); statErr == nil {
			return fmt.Errorf("move the incomplete recordings path %q aside before retrying: %w", target, err)
		}
	}
	return err
}
