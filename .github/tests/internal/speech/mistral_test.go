// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package speech

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type mistralTestTransport func(*http.Request) (*http.Response, error)

// Use a canned server response behind the normal request construction
func (f mistralTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVoxtralTranscriptionResponse(t *testing.T) {
	pcm := []byte{1, 2, 3, 4}
	for _, tc := range []struct {
		name, response, translation, want string
		invalid                           bool
	}{
		{"english", `{"text":" Jetway ","language":"en"}`, "", "jetway", false},
		{"empty", `{"text":" "}`, "", "", false},
		{"truncated", `{"text":"pushback","finish_reason":"length"}`, "", "", false},
		{"missing text", `{}`, "", "", true},
		{"wrong text type", `{"text":["unexpected"]}`, "", "", true},
		{"translate", `{"text":"alimentation au sol","language":"fr"}`, `{"choices":[{"finish_reason":"stop","message":{"content":"{\"text\":\"Ground power\"}"}}]}`, "ground power", false},
		{"partial translation", `{"text":"alimentation au sol","language":"fr"}`, `{"choices":[{"finish_reason":"length","message":{"content":"{\"text\":\"Ground power\"}"}}]}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			// Exercise the real request path without sending recordings or using a real key
			calls := 0
			http.DefaultTransport = mistralTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				body := tc.response
				switch calls {
				case 1:
					if r.URL.Path != "/v1/audio/transcriptions" || r.ParseMultipartForm(1<<20) != nil {
						t.Fatalf("expected multipart transcription, got %s", r.URL.Path)
					}
					if r.FormValue("model") != "voxtral-small-transcribe-3" || r.FormValue("temperature") != "0" || r.FormValue("return_language") != "true" || r.FormValue("context_bias") != "" {
						t.Fatalf("unexpected transcription fields: %v", r.MultipartForm.Value)
					}
					file, _, err := r.FormFile("file")
					if err != nil {
						t.Fatal(err)
					}
					defer file.Close()
					audio, err := io.ReadAll(file)
					if err != nil || !bytes.Equal(audio, wave(pcm)) {
						t.Fatal("unexpected WAV upload")
					}
				// Inspect the translation body to catch any accidental second upload of the audio.
				case 2:
					var payload struct {
						Messages []struct{ Role, Content string }
					}
					if r.URL.Path != "/v1/chat/completions" || json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload.Messages) != 2 || payload.Messages[1].Content != "alimentation au sol" {
						t.Fatal("expected text-only translation of the transcript")
					}
					body = tc.translation
				default:
					t.Fatal("unexpected extra request")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			transcript, err := Voxtral(t.Context(), "dummy-test-key", "voxtral-small-latest", pcm)
			// A second request should occur only for fixtures that require translation.
			if (calls == 2) != (tc.translation != "") {
				t.Fatalf("unexpected request count: %d", calls)
			}
			// Keep unexpected response content out of the error
			if tc.invalid {
				if err == nil || err.Error() != "Mistral returned an invalid transcription response" {
					t.Fatalf("expected sanitized transcription error, got %v", err)
				}
			} else if err != nil || transcript != tc.want {
				t.Fatalf("transcript = %q, error = %v, want %q", transcript, err, tc.want)
			}
		})
	}
}
