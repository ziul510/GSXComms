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
		shortPress, release := context.WithTimeout(t.Context(), 399*time.Millisecond)
		defer release()
		if waitForPTTStatic(shortPress) || time.Since(start) != 399*time.Millisecond {
			t.Fatal("releasing at 399 ms must cancel static immediately")
		}

		start = time.Now()
		if !waitForPTTStatic(t.Context()) || time.Since(start) != 400*time.Millisecond {
			t.Fatal("a held button must allow static at 400 ms")
		}
	})
}
