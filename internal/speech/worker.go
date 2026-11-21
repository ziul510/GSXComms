// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package speech

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const Version = 2
const MaxFrame = 16 << 20

// The parent and both native workers share this request and reply payload
type Frame struct {
	Version    int               `json:"version"`
	ID         uint64            `json:"id"`
	Op         string            `json:"op"`
	Paths      map[string]string `json:"paths,omitempty"`
	Hotwords   []string          `json:"hotwords,omitempty"`
	PCM        []byte            `json:"pcm,omitempty"`
	Text       string            `json:"text,omitempty"`
	SampleRate int               `json:"sample_rate,omitempty"`
	Runtime    string            `json:"runtime,omitempty"`
	Error      string            `json:"error,omitempty"`
}

func Read(r io.Reader) (Frame, error) {
	var f Frame
	// Read the little-endian length first, then exactly one JSON message
	var size uint32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return f, err
	}
	// Reject corrupt lengths before allocating space for the pipe payload
	if size == 0 || size > MaxFrame {
		return f, errors.New("invalid worker frame size")
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, err
	}
	// Reject a packaged worker that speaks a different protocol version
	if f.Version != Version {
		return f, errors.New("incompatible speech worker version")
	}
	return f, nil
}

func Write(w io.Writer, f Frame) error {
	// All callers use the same header and protocol version
	f.Version = Version
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	// Enforce the size limit before writing a header the receiving side would reject
	if len(b) > MaxFrame {
		return errors.New("speech worker frame is too large")
	}
	// Put the byte count ahead of the JSON so messages can share the same pipe
	if err := binary.Write(w, binary.LittleEndian, uint32(len(b))); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

type Worker struct {
	mu       sync.Mutex
	kind     string
	cmd      *exec.Cmd
	in       io.WriteCloser
	out      io.ReadCloser
	job      windows.Handle
	sequence uint64
	loaded   string
	runtime  string
	failed   time.Time
	failures int
}

// Construction records the provider The subprocess starts on the first load
func NewWorker(kind string) *Worker { return &Worker{kind: kind} }

func (w *Worker) Load(ctx context.Context, signature string, paths map[string]string, hotwords []string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.cmd == nil {
		if err := w.start(ctx); err != nil {
			return err
		}
	}
	// Keep the native model loaded across transmissions with the same inputs
	if w.loaded == signature {
		return nil
	}
	r, err := w.exchange(ctx, 120*time.Second, Frame{Op: "load", Paths: paths, Hotwords: hotwords})
	if err != nil {
		return err
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	// Only cache the signature after the worker confirms it opened the model
	w.loaded = signature
	w.runtime = r.Runtime
	return nil
}

func (w *Worker) Runtime() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runtime
}

func (w *Worker) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd == nil || w.loaded == "" {
		return "", errors.New("speech model is unloaded")
	}
	// Serialize requests through the worker's single stdin and stdout pair
	resp, err := w.exchange(ctx, 240*time.Second, Frame{Op: "transcribe", SampleRate: 16000, PCM: pcm})
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	return resp.Text, nil
}

// FilterSpeech uses the same local voice detector as the offline recognizers
func (w *Worker) FilterSpeech(ctx context.Context, pcm []byte) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd == nil {
		if err := w.start(ctx); err != nil {
			return nil, err
		}
	}
	resp, err := w.exchange(ctx, 30*time.Second, Frame{Op: "filter_speech", SampleRate: 16000, PCM: pcm})
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.PCM, nil
}

func (w *Worker) start(ctx context.Context) error {
	// Require an explicit reload after repeated failures to avoid a crash loop
	if w.failures >= 2 {
		return errors.New("speech worker stopped after repeated failures; reload the model to retry")
	}
	// Back off briefly after a crash before starting another child
	if !w.failed.IsZero() && time.Since(w.failed) < time.Second {
		t := time.NewTimer(time.Second - time.Since(w.failed))
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := w.launch(); err != nil {
		w.fail()
		return err
	}
	// Check that the child loaded its native runtime before sending a model
	r, err := w.exchange(ctx, 5*time.Second, Frame{Op: "hello"})
	if err != nil {
		w.stop()
		return err
	}
	if r.Error != "" {
		w.fail()
		return fmt.Errorf("speech worker handshake failed: %s", r.Error)
	}
	return nil
}

func (w *Worker) launch() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Each provider has its own executable directory and native libraries
	base := filepath.Join(filepath.Dir(exe), "workers", w.kind)
	cmd := exec.Command(filepath.Join(base, "gsxspeech.exe"))
	cmd.Dir = base
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return err
	}
	// Create the Windows job before launch so the worker can be tied to the parent
	job, err := createJob()
	if err != nil {
		in.Close()
		out.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		in.Close()
		out.Close()
		return err
	}
	// Record resources before assignment so a failed assignment can still be cleaned up
	w.cmd, w.in, w.out, w.job = cmd, in, out, job
	return assignJob(job, uint32(cmd.Process.Pid))
}

func (w *Worker) exchange(ctx context.Context, timeout time.Duration, req Frame) (Frame, error) {
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// A monotonically increasing ID separates calls even when the worker gets restarted
	w.sequence++
	req.ID = w.sequence
	type result struct {
		reply Frame
		err   error
	}
	// The pipe goroutine may still need to send after a timeout returns
	resultCh := make(chan result, 1)
	in, out := w.in, w.out
	go func() {
		if err := Write(in, req); err != nil {
			resultCh <- result{err: err}
			return
		}
		f, err := Read(out)
		resultCh <- result{f, err}
	}()
	select {
	case r := <-resultCh:
		// A reply has to belong to the request currently using the pipe
		if r.err == nil && r.reply.ID == req.ID && r.reply.Op == req.Op {
			return r.reply, nil
		}
		w.fail()
		return Frame{}, errors.New("speech worker exited or returned an invalid response")
	case <-ctx.Done():
		// Give in-flight inference a short chance to finish before stopping the process
		if req.Op != "shutdown" {
			t := time.NewTimer(2 * time.Second)
			select {
			case <-resultCh:
			case <-t.C:
			}
			t.Stop()
		}
		// Count a timeout toward the retry budget A canceled caller only stops the process
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			w.fail()
		} else {
			w.stop()
		}
		return Frame{}, fmt.Errorf("speech %s: %w", req.Op, ctx.Err())
	}
}

func (w *Worker) fail() {
	// Only failures within a minute of each other share the retry budget
	if time.Since(w.failed) >= time.Minute {
		w.failures = 0
	}
	w.failed = time.Now()
	w.failures++
	w.stop()
}

func (w *Worker) stop() {
	if w.cmd == nil {
		return
	}
	// Close the job and kill any child that wasn't assigned to it successfully
	_ = windows.CloseHandle(w.job)
	_ = w.cmd.Process.Kill()
	_ = w.in.Close()
	_ = w.out.Close()
	_ = w.cmd.Wait()
	// Clear the signature along with the child so the next request loads a fresh model
	w.cmd, w.in, w.out, w.job, w.loaded = nil, nil, nil, 0, ""
	w.runtime = ""
}

// Reset waits for the current pipe exchange before asking the worker to shut down
func (w *Worker) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Offer a graceful shutdown before forcing the child down and clearing its retry state
	if w.cmd != nil {
		_, _ = w.exchange(context.Background(), 2*time.Second, Frame{Op: "shutdown"})
	}
	w.stop()
	w.failed, w.failures = time.Time{}, 0
}

func (w *Worker) Synthesize(ctx context.Context, text string) ([]byte, int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd == nil || w.loaded == "" {
		return nil, 0, errors.New("Kokoro model is unloaded")
	}
	resp, err := w.exchange(ctx, 240*time.Second, Frame{Op: "synthesize", Text: text})
	if err != nil {
		return nil, 0, err
	}
	if resp.Error != "" {
		return nil, 0, errors.New(resp.Error)
	}
	// Playback expects the Kokoro worker's fixed rate and complete PCM16 samples
	if resp.SampleRate != 24000 || len(resp.PCM)%2 != 0 {
		return nil, 0, errors.New("Kokoro worker returned invalid PCM")
	}
	return resp.PCM, resp.SampleRate, nil
}
