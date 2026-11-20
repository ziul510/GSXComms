// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package speech

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

//go:embed voxtral-prompt.txt
var prompt string

const mistralAPI = "https://api.mistral.ai/v1/"

var errInvalidTranscription = errors.New("Mistral returned an invalid transcription response")

func Usable(pcm []byte) bool {
	// At 16 kHz PCM16, 8000 bytes is a quarter second Partial samples are invalid
	if len(pcm) < 8000 || len(pcm)%2 != 0 {
		return false
	}
	// Read signed samples so loud negative peaks can pass the amplitude gate
	for i := 0; i < len(pcm); i += 2 {
		s := int16(binary.LittleEndian.Uint16(pcm[i:]))
		if s >= 350 || s <= -350 {
			return true
		}
	}
	return false
}

func wave(pcm []byte) []byte {
	// Wrap the captured mono PCM16 in a standard 44-byte WAV header for the API
	wav := make([]byte, 44+len(pcm))
	copy(wav, "RIFF")
	// The RIFF chunk length excludes the first eight header bytes
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(pcm)+36))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 16000)
	binary.LittleEndian.PutUint32(wav[28:], 32000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(len(pcm)))
	copy(wav[44:], pcm)
	return wav
}

func request(ctx context.Context, key, method, endpoint string, data []byte, timeout time.Duration) ([]byte, error) {
	// Fail before building a request when no vault credential is available
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("Mistral API key is not set")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, mistralAPI+endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("Mistral request could not be prepared")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Keep the token and audio on the configured endpoint by rejecting redirects
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Mistral network error")
	}
	defer resp.Body.Close()
	// Translate service errors into UI messages without exposing the response body
	switch resp.StatusCode {
	case http.StatusOK:
		return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, errors.New("Mistral rejected the API key")
	case http.StatusTooManyRequests:
		return nil, errors.New("Mistral rate limit reached; wait and try again")
	default:
		return nil, fmt.Errorf("Mistral request failed with HTTP %d", resp.StatusCode)
	}
}

// Check the credential with the models endpoint so validation uploads no recording
func ValidateMistral(ctx context.Context, key string) error {
	_, err := request(ctx, key, http.MethodGet, "models", nil, 10*time.Second)
	return err
}

func Voxtral(ctx context.Context, key, model, table string, pcm []byte) (string, error) {
	// Apply the upload gate here even if a caller skipped the UI's check
	if !Usable(pcm) {
		return "", nil
	}
	if model = strings.TrimSpace(model); model == "" {
		model = "voxtral-small-latest"
	}
	instruction := strings.TrimSpace(prompt)
	// Use the same service terms the local command router can recognize
	if table != "" {
		instruction += "\n\nHotwords table from words.json (use ONLY these EXACT English words for services):\n" + table
	}
	payload := map[string]any{"model": model, "messages": []any{map[string]any{
		"role": "user", "content": []any{
			// JSON encodes the WAV byte slice as the base64 audio field
			map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": wave(pcm), "format": "wav"}},
			map[string]any{"type": "text", "text": instruction},
		},
	}}}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", errors.New("could not encode Mistral audio")
	}
	data, err := request(ctx, key, http.MethodPost, "chat/completions", body, 30*time.Second)
	if err != nil {
		return "", err
	}
	var result struct {
		Choices []struct {
			Message struct{ Content json.RawMessage }
		}
	}
	if json.Unmarshal(data, &result) != nil {
		return "", errInvalidTranscription
	}
	// A successful response can still contain no transcript
	if len(result.Choices) == 0 || result.Choices[0].Message.Content == nil {
		return "", nil
	}
	content := result.Choices[0].Message.Content
	var transcript string
	// The API can return plain text or an array of typed content parts
	if json.Unmarshal(content, &transcript) != nil {
		var parts []struct{ Type, Text string }
		if json.Unmarshal(content, &parts) != nil {
			return "", errInvalidTranscription
		}
		var all strings.Builder
		// Join text parts in order and skip any non-text content
		for _, part := range parts {
			if part.Type == "text" {
				all.WriteString(part.Text)
			}
		}
		transcript = all.String()
	}
	return strings.ToLower(strings.TrimSpace(transcript)), nil
}
