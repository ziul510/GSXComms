// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package speech

import (
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"
)

type mistralTestTransport func(*http.Request) (*http.Response, error)

func (f mistralTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVoxtralTranscriptionResponse(t *testing.T) {
	pcm := make([]byte, 8000)
	binary.LittleEndian.PutUint16(pcm, 500)
	for _, tc := range []struct {
		name, message, transcript string
		invalid                   bool
	}{
		{"string", `{"content":" Jetway "}`, "jetway", false},
		{"text parts", `{"content":[{"type":"text","text":" Ground"},{"type":"text","text":" power "}]}`, "ground power", false},
		{"empty", `{"content":null}`, "", false},
		{"omitted", `{}`, "", false},
		{"wrong content type", `{"content":{"text":"unexpected"}}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			http.DefaultTransport = mistralTestTransport(func(*http.Request) (*http.Response, error) {
				body := `{"choices":[{"message":` + tc.message + `}]}`
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			transcript, err := Voxtral(t.Context(), "dummy-test-key", "", "", pcm)
			if tc.invalid {
				if err == nil || err.Error() != "Mistral returned an invalid transcription response" {
					t.Fatalf("expected sanitized transcription error, got %v", err)
				}
			} else if err != nil || transcript != tc.transcript {
				t.Fatalf("transcript = %q, error = %v, want %q", transcript, err, tc.transcript)
			}
		})
	}
}
