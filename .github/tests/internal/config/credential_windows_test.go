// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/rand"
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsCredentialRoundTrip(t *testing.T) {
	name := keyTarget + "/test-" + rand.Text()
	const fixture = "credential-manager-round-trip"
	if err := writeKey(name, fixture); err != nil {
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
