// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

type transportFunc func(*http.Request) (*http.Response, error)

// Inject the response through the HTTP client's normal transport path
func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestResumeAndPublishModel(t *testing.T) {
	const content = "example data from a pinned revision"
	digest := sha256.Sum256([]byte(content))
	f := File{"models/fixture", int64(len(content)), fmt.Sprintf("%x", digest)}
	m := Model{"test", "owner/example", "pinned", []File{f}}
	dir := t.TempDir()
	partial := filepath.Join(m.Directory(dir)+".partial", "models", "fixture")
	if err := os.MkdirAll(filepath.Dir(partial), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte(content[:7]), 0600); err != nil {
		t.Fatal(err)
	}
	original := client
	defer func() { client = original }()
	called := false
	client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		if got := req.Header.Get("Range"); got != "bytes=7-" {
			t.Errorf("Range = %q", got)
		}
		resp := &http.Response{StatusCode: http.StatusPartialContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(content[7:]))}
		resp.Header.Set("Content-Range", fmt.Sprintf("bytes 7-%d/%d", len(content)-1, len(content)))
		return resp, nil
	})}
	// A staged model must stay unavailable for inference until publishing succeeds
	if m.Ready(dir) {
		t.Fatal("staging data became usable before verification")
	}
	if err := m.Download(context.Background(), dir, func(int64, int64) {}); err != nil {
		t.Fatal(err)
	}
	// Check the published file so a valid marker alone cannot pass the resume test.
	if !called || !m.Ready(dir) || !f.matches(m.Paths(dir)["fixture"]) {
		t.Fatal("resumed model not committed")
	}
}

func TestActiveDownloadOwnsItsStagingDirectory(t *testing.T) {
	const body = "model"
	f := File{"fixture", int64(len(body)), fmt.Sprintf("%x", sha256.Sum256([]byte(body)))}
	m := Model{"test", "owner/example", "pinned", []File{f}}
	dir := t.TempDir()
	t.Setenv("HF_HUB_CACHE", t.TempDir())

	requested := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	original := client
	client = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		close(requested)
		<-release
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	defer func() { client = original }()
	go func() { finished <- m.Download(context.Background(), dir, func(int64, int64) {}) }()
	select {
	case <-requested:
	case err := <-finished:
		t.Fatalf("download stopped before requesting the file: %v", err)
	}
	// Pause the download so deletion hits the lock while staging is active
	cacheVisible := m.Cached(dir) && !m.Ready(dir)
	// The first delete attempts to acquire the same lock as the paused download
	deleteErr := m.Delete(dir)
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if !cacheVisible || deleteErr == nil || !m.Ready(dir) {
		t.Fatalf("staged=%v concurrent delete=%v ready=%v", cacheVisible, deleteErr, m.Ready(dir))
	}
	if err := m.Delete(dir); err != nil || m.Cached(dir) {
		t.Fatalf("delete after publication: %v", err)
	}
}

func TestIncompleteModelCanBeDeletedBeforeRetry(t *testing.T) {
	m := Model{"test", "owner/example", "pinned", []File{{"fixture", 5, strings.Repeat("0", 64)}}}
	dir := t.TempDir()
	// A directory can exist without verified files The UI must offer deletion in that state
	if err := os.MkdirAll(m.Directory(dir), 0700); err != nil {
		t.Fatal(err)
	}
	if !m.Cached(dir) || m.Ready(dir) {
		t.Fatal("incomplete model must be removable")
	}
	if err := m.Download(context.Background(), dir, func(int64, int64) {}); err == nil {
		t.Fatal("expected a recovery error for the incomplete model")
	}
	if _, err := os.Stat(m.Directory(dir) + ".partial"); !os.IsNotExist(err) {
		t.Fatal("retry started staging before asking to remove the incomplete cache")
	}
	if err := m.Delete(dir); err != nil || m.Cached(dir) {
		t.Fatalf("delete incomplete cache: %v", err)
	}
}

func TestTruncatedDownloadRetainsVerifiedResumeOffset(t *testing.T) {
	const body = "a model fixture"
	f := File{"fixture", int64(len(body)), fmt.Sprintf("%x", sha256.Sum256([]byte(body)))}
	m := Model{"test", "owner/example", "pinned", []File{f}}
	dir := t.TempDir()
	// Use an empty cache so both attempts exercise the HTTP download path.
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	original := client
	defer func() { client = original }()
	requests := 0
	client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		// Simulate an early EOF and leave three bytes for the later resume request
		if requests == 1 {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body[:3]))}, nil
		}
		if req.Header.Get("Range") != "bytes=3-" {
			t.Fatalf("unexpected retry range %q", req.Header.Get("Range"))
		}
		resp := &http.Response{StatusCode: http.StatusPartialContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body[3:]))}
		resp.Header.Set("Content-Range", fmt.Sprintf("bytes 3-%d/%d", len(body)-1, len(body)))
		return resp, nil
	})}
	progress := func(int64, int64) {}
	if err := m.Download(context.Background(), dir, progress); err == nil || m.Ready(dir) {
		t.Fatal("truncated download did not fail")
	}
	if err := m.Download(context.Background(), dir, progress); err != nil || !m.Ready(dir) {
		t.Fatalf("resuming truncated download: %v", err)
	}
}

func TestChecksumMismatchIsNotPublished(t *testing.T) {
	body := "bad-data"
	// Use valid length and the wrong digest so only the checksum check can catch the file
	f := File{"fixture", int64(len(body)), strings.Repeat("0", 64)}
	m := Model{"test", "owner/example", "pinned", []File{f}}
	dir := t.TempDir()
	original := client
	defer func() { client = original }()
	client = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if err := m.Download(context.Background(), dir, func(int64, int64) {}); err == nil || m.Ready(dir) {
		t.Fatal("bad model accepted")
	}
	if _, err := os.Stat(m.Directory(dir) + ".partial/fixture"); !os.IsNotExist(err) {
		t.Fatal("bad file remained cached")
	}
}

func TestCacheImportPreservesResumableModel(t *testing.T) {
	const body = "model fixture"
	f := File{"fixture", int64(len(body)), fmt.Sprintf("%x", sha256.Sum256([]byte(body)))}
	m := Model{"test", "owner/example", "pinned", []File{f}}
	for name, cacheBody := range map[string]string{"verified": body, "corrupt": strings.Repeat("x", len(body))} {
		t.Run(name, func(t *testing.T) {
			dir, cache := t.TempDir(), t.TempDir()
			t.Setenv("HF_HUB_CACHE", cache)
			partialDir := m.Directory(dir) + ".partial"
			snapshot := filepath.Join(cache, "models--owner--example", "snapshots", m.Revision)
			for folder, contents := range map[string]string{partialDir: body[:3], snapshot: cacheBody} {
				if err := os.MkdirAll(folder, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(folder, f.Path), []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			original := client
			t.Cleanup(func() { client = original })
			requests := 0
			client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if got := req.Header.Get("Range"); got != "bytes=3-" {
					t.Errorf("Range = %q, want bytes=3-", got)
				}
				resp := &http.Response{StatusCode: http.StatusPartialContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body[3:]))}
				resp.Header.Set("Content-Range", fmt.Sprintf("bytes 3-%d/%d", len(body)-1, len(body)))
				return resp, nil
			})}
			if err := m.Download(t.Context(), dir, func(int64, int64) {}); err != nil || !f.matches(m.Paths(dir)[f.Path]) {
				t.Fatalf("publishing model: %v", err)
			}
			// A valid cache can skip HTTP A corrupt cache must use the existing partial download
			wantRequests := 0
			if name == "corrupt" {
				wantRequests = 1
			}
			if requests != wantRequests {
				t.Fatalf("HTTP requests = %d, want %d", requests, wantRequests)
			}
		})
	}
}

func TestVerifyRejectsSameSizeModelCorruption(t *testing.T) {
	m := Model{"test", "owner/example", "pinned", []File{{"fixture", 4, fmt.Sprintf("%x", sha256.Sum256([]byte("good")))}}}
	dir := t.TempDir()
	if err := os.MkdirAll(m.Directory(dir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Paths(dir)["fixture"], []byte("bad!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Directory(dir), ".verified"), []byte(m.Revision), 0600); err != nil {
		t.Fatal(err)
	}
	if !m.Ready(dir) {
		t.Fatal("fixture should pass the lightweight UI readiness check")
	}
	// A Windows reader can temporarily block removal of the verification marker
	marker := windows.StringToUTF16Ptr(filepath.Join(m.Directory(dir), ".verified"))
	handle, err := windows.CreateFile(marker, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	verifyErr := m.Verify(t.Context(), dir)
	_ = windows.CloseHandle(handle)
	if verifyErr == nil || m.Ready(dir) || !m.Cached(dir) {
		t.Fatalf("corrupted model should be available for deletion only: %v", verifyErr)
	}
	if err := m.Delete(dir); err != nil || m.Cached(dir) {
		t.Fatalf("deletion should clear the invalid bundle: %v", err)
	}
}

func TestVerifyKeepsMarkerOnTemporaryReadFailure(t *testing.T) {
	m := Model{"test", "owner/example", "pinned", []File{{"fixture", 4, fmt.Sprintf("%x", sha256.Sum256([]byte("good")))}}}
	dir := t.TempDir()
	if err := os.MkdirAll(m.Directory(dir), 0700); err != nil {
		t.Fatal(err)
	}
	path := m.Paths(dir)["fixture"]
	if err := os.WriteFile(path, []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(m.Directory(dir), ".verified")
	if err := os.WriteFile(marker, []byte(m.Revision), 0600); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent process can block the read without damaging a valid model.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	verifyErr := m.Verify(t.Context(), dir)
	_ = windows.CloseHandle(handle)
	if !errors.Is(verifyErr, windows.ERROR_SHARING_VIOLATION) || !m.Ready(dir) {
		t.Fatalf("healthy model was invalidated after a temporary read error: %v", verifyErr)
	}
	if err := m.Verify(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
}
