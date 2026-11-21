// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestWaitForPTTStatic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		shortPress, release := context.WithTimeout(t.Context(), 149*time.Millisecond)
		defer release()
		if waitForPTTStatic(shortPress) || time.Since(start) != 149*time.Millisecond {
			t.Fatal("releasing at 149 ms must cancel static immediately")
		}

		start = time.Now()
		if !waitForPTTStatic(t.Context()) || time.Since(start) != 150*time.Millisecond {
			t.Fatal("a held button must allow static at 150 ms")
		}
	})
}
