// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsCredentialRoundTrip(t *testing.T) {
	// Use a separate credential target so cleanup cannot delete the user's key.
	name := keyTarget + "/test-" + rand.Text()
	const fixture = "credential-manager-round-trip"
	if err := writeKey(name, fixture); err != nil {
		// Headless Windows sessions can lack a credential vault.
		if errors.Is(err, windows.ERROR_NO_SUCH_LOGON_SESSION) {
			t.Skip("Windows session has no Credential Manager vault")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writeKey(name, ""); err != nil {
			t.Error(err)
		}
	})
	if value, err := readKey(name); err != nil || value != fixture {
		t.Fatalf("Credential Manager read-back failed: %v", err)
	}
}

func TestSettingsDirectoryLock(t *testing.T) {
	dir := t.TempDir()
	first, err := LockSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LockSettings(dir)
	first()
	if err == nil {
		second()
		t.Fatal("a second writer acquired the same settings directory")
	}
	reopened, err := LockSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened()
}

func TestCorruptSettingsBackupIsProtected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFile)
	original := []byte(`{"mistral_api_key":"fixture-only",`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected a corrupt settings diagnostic")
	}
	fixtures, err := filepath.Glob(path + ".corrupt.*.dpapi")
	if err != nil || len(fixtures) != 1 {
		t.Fatalf("encrypted backups = %v, err = %v", fixtures, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext settings remain: %v", err)
	}
	data, err := os.ReadFile(fixtures[0])
	if err != nil || len(data) == 0 || bytes.Contains(data, original) {
		t.Fatalf("invalid protected backup: %v", err)
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		t.Fatal(err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	if !bytes.Equal(unsafe.Slice(out.Data, int(out.Size)), original) {
		t.Fatal("could not recover the original settings from the protected backup")
	}
}
