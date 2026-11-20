// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ziul510/GSXCommsGo/internal/config"
	"golang.org/x/sys/windows"
)

//go:embed manifest.json
var manifest []byte

type File struct {
	Path   string
	Bytes  int64
	SHA256 string
}
type Model struct {
	Name, Repo, Revision string
	Files                []File
}

var catalog map[string]Model

func init() {
	if err := json.Unmarshal(manifest, &catalog); err != nil {
		panic(err)
	}
}
func Lookup(key string) (Model, bool) { m, ok := catalog[key]; return m, ok }

func (m Model) Directory(data string) string {
	return filepath.Join(data, "models", m.Name+"-"+m.Revision)
}
func (m Model) Paths(data string) map[string]string {
	out := make(map[string]string, len(m.Files))
	for _, f := range m.Files {
		out[filepath.Base(f.Path)] = filepath.Join(m.Directory(data), filepath.FromSlash(f.Path))
	}
	return out
}

func (m Model) Ready(data string) bool {
	for _, f := range m.Files {
		stat, err := os.Stat(filepath.Join(m.Directory(data), filepath.FromSlash(f.Path)))
		if err != nil || !stat.Mode().IsRegular() || stat.Size() != f.Bytes {
			return false
		}
	}
	// Use the download's verification marker to avoid hashing models on every redraw.
	mark, err := os.ReadFile(filepath.Join(m.Directory(data), ".verified"))
	return err == nil && strings.TrimSpace(string(mark)) == m.Revision
}

func (m Model) Cached(data string) bool {
	final := m.Directory(data)
	for _, path := range []string{final, final + ".partial"} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func (m Model) lock(data string) (windows.Handle, error) {
	final := m.Directory(data)
	if err := os.MkdirAll(filepath.Dir(final), 0700); err != nil {
		return 0, err
	}
	path, err := windows.UTF16PtrFromString(final + ".lock")
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(path, windows.GENERIC_WRITE|windows.DELETE, 0, nil, windows.OPEN_ALWAYS, windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return 0, errors.New("another operation is using this model")
	}
	return handle, err
}

func cacheRoot() string {
	if p := os.Getenv("HF_HUB_CACHE"); p != "" {
		return p
	}
	if p := os.Getenv("HUGGINGFACE_HUB_CACHE"); p != "" {
		return p
	}
	if p := os.Getenv("HF_HOME"); p != "" {
		return filepath.Join(p, "hub")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "huggingface", "hub")
}

func (m Model) Download(ctx context.Context, data string, progress func(int64, int64)) error {
	handle, err := m.lock(data)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	final := m.Directory(data)
	if m.Ready(data) {
		return nil
	}
	if _, err := os.Stat(final); err == nil {
		return errors.New("model files are incomplete; delete the model before retrying")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	staging := final + ".partial"
	if err := os.MkdirAll(staging, 0700); err != nil {
		return err
	}
	var total, needed, done int64
	for _, file := range m.Files {
		total += file.Bytes
		stat, err := os.Stat(filepath.Join(staging, filepath.FromSlash(file.Path)))
		remaining := file.Bytes
		if err == nil && stat.Mode().IsRegular() && stat.Size() <= file.Bytes {
			remaining -= stat.Size()
		}
		needed += remaining
	}
	path, err := windows.UTF16PtrFromString(staging)
	if err != nil {
		return err
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(path, &free, nil, nil); err != nil {
		return err
	}
	if free < uint64(needed)+(8<<20) {
		return errors.New("not enough free disk space for this model")
	}
	cache := filepath.Join(cacheRoot(), "models--"+strings.ReplaceAll(m.Repo, "/", "--"), "snapshots", m.Revision)
	for _, file := range m.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		dest := filepath.Join(staging, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		if !file.matches(dest) {
			local := filepath.Join(cache, filepath.FromSlash(file.Path))
			verified := false
			// Check the cache before it can overwrite a resumable download.
			if file.matches(local) {
				if err := copyFile(ctx, local, dest, func(w int64) { progress(done+w, total) }); err != nil {
					return err
				}
				verified = file.matches(dest)
			}
			if !verified {
				if err := m.fetch(ctx, file, dest, func(w int64) { progress(done+w, total) }); err != nil {
					return err
				}
				if !file.matches(dest) {
					_ = os.Remove(dest)
					return fmt.Errorf("checksum mismatch for %s", file.Path)
				}
			}
		}
		done += file.Bytes
		progress(done, total)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := config.WriteAtomic(filepath.Join(staging, ".verified"), []byte(m.Revision)); err != nil {
		return err
	}
	return os.Rename(staging, final)
}

func (m Model) Delete(data string) error {
	handle, err := m.lock(data)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return errors.Join(os.RemoveAll(m.Directory(data)), os.RemoveAll(m.Directory(data)+".partial"))
}

func (f File) matches(path string) bool {
	handle, err := os.Open(path)
	if err != nil {
		return false
	}
	defer handle.Close()
	stat, err := handle.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != f.Bytes {
		return false
	}
	hash := sha256.New()
	_, err = io.Copy(hash, handle)
	return err == nil && hex.EncodeToString(hash.Sum(nil)) == f.SHA256
}

var client = &http.Client{
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 30 * time.Second, TLSHandshakeTimeout: 15 * time.Second},
}

func (m Model) fetch(ctx context.Context, f File, dest string, progress func(int64)) error {
	endpoint := url.URL{Scheme: "https", Host: "huggingface.co", Path: "/" + m.Repo + "/resolve/" + m.Revision + "/" + f.Path}
	var offset int64
	if st, err := os.Stat(dest); err == nil && st.Size() < f.Bytes {
		offset = st.Size()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	// Request uncompressed bytes so resume offsets match the staged file.
	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusOK:
		offset = 0
		flags |= os.O_TRUNC
	case http.StatusPartialContent:
		expected := fmt.Sprintf("bytes %d-%d/%d", offset, f.Bytes-1, f.Bytes)
		if offset == 0 || resp.Header.Get("Content-Range") != expected {
			return errors.New("invalid resume range")
		}
		flags |= os.O_APPEND
	default:
		return fmt.Errorf("download of %s returned HTTP %d", f.Path, resp.StatusCode)
	}
	out, err := os.OpenFile(dest, flags, 0600)
	if err != nil {
		return err
	}
	remaining := f.Bytes - offset
	written, err := copyProgress(ctx, out, io.LimitReader(resp.Body, remaining+1), func(w int64) { progress(offset + w) })
	if err == nil && written != remaining {
		err = io.ErrUnexpectedEOF
	}
	return errors.Join(err, out.Sync(), out.Close())
}

func copyFile(ctx context.Context, src, dst string, progress func(int64)) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = copyProgress(ctx, out, in, progress)
	return errors.Join(err, out.Sync(), out.Close())
}

func copyProgress(ctx context.Context, dst io.Writer, src io.Reader, progress func(int64)) (int64, error) {
	var written int64
	var last time.Time
	block := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, err := src.Read(block)
		if n > 0 {
			m, werr := dst.Write(block[:n])
			written += int64(m)
			if time.Since(last) > 150*time.Millisecond {
				progress(written)
				last = time.Now()
			}
			if werr != nil {
				return written, werr
			}
			if m != n {
				return written, io.ErrShortWrite
			}
		}
		if errors.Is(err, io.EOF) {
			progress(written)
			return written, nil
		}
		if err != nil {
			return written, err
		}
	}
}
