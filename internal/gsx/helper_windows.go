// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

type helperRequest struct {
	Op        string  `json:"op"`
	Name      string  `json:"name"`
	Value     float64 `json:"value"`
	TimeoutMS int64   `json:"timeout_ms"`
}

type helperReply struct {
	Value     *float64 `json:"value"`
	Error     string   `json:"error"`
	Fatal     bool     `json:"fatal"`
	Uncertain bool     `json:"uncertain"`
}

// Serialize individual exchanges so telemetry can run between menu operations
type helperTransport struct {
	gate chan struct{}
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.ReadCloser
	send *json.Encoder
	recv *json.Decoder
}

// ErrWriteUncertain means the simulator may already have acted on the write.
var ErrWriteUncertain = errors.New("SimConnect write outcome unknown")

func Connect(ctx context.Context) (Transport, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	// Resolve the helper beside the executable so the ZIP can launch from any working directory
	cmd := exec.Command(filepath.Join(filepath.Dir(exe), "gsxsimconnect.exe"))
	return connectHelper(ctx, cmd)
}

func connectHelper(ctx context.Context, cmd *exec.Cmd) (Transport, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, fmt.Errorf("start SimConnect helper: %w", err)
	}
	transport := &helperTransport{gate: make(chan struct{}, 1), cmd: cmd, in: in, out: out, send: json.NewEncoder(in), recv: json.NewDecoder(out)}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Read the helper's unsolicited handshake before exposing the transport.
	if _, err := transport.exchange(ctx, nil, nil); err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("SimConnect unavailable: %w", err)
	}
	return transport, nil
}

func (t *helperTransport) exchange(ctx context.Context, req *helperRequest, check func() error) (helperReply, error) {
	select {
	// Hold the gate across the round trip so telemetry and menu requests cannot trade replies.
	case t.gate <- struct{}{}:
		defer func() { <-t.gate }()
	case <-ctx.Done():
		return helperReply{}, ctx.Err()
	}
	if t.cmd == nil {
		return helperReply{}, ErrDisconnected
	}
	if err := ctx.Err(); err != nil {
		return helperReply{}, err
	}
	if check != nil {
		if err := check(); err != nil {
			return helperReply{}, err
		}
		if err := ctx.Err(); err != nil {
			return helperReply{}, err
		}
	}
	if req != nil && req.Op == "get" {
		req.TimeoutMS = 350
		if deadline, ok := ctx.Deadline(); ok {
			// Leave time for the helper to return its reply before the caller times out.
			remaining := time.Until(deadline) - 30*time.Millisecond
			if remaining <= 0 {
				return helperReply{}, context.DeadlineExceeded
			}
			req.TimeoutMS = min(req.TimeoutMS, remaining.Milliseconds())
		}
	}
	if req != nil {
		// Bound stalled pipe writes and reads even when the console supplies no deadline
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Second)
		defer cancel()
	}
	type result struct {
		reply helperReply
		err   error
	}
	sent := make(chan result, 1)
	go func() {
		if req != nil {
			if err := t.send.Encode(req); err != nil {
				sent <- result{err: err}
				return
			}
		}
		var reply helperReply
		err := t.recv.Decode(&reply)
		sent <- result{reply: reply, err: err}
	}()
	fail := func(err error) (helperReply, error) {
		t.stop()
		// A failed initial handshake has no live connection to lose
		if req == nil {
			return helperReply{}, err
		}
		// A lost reply can mean the simulator already processed the write
		if req.Op == "set" {
			err = errors.Join(ErrWriteUncertain, err)
		}
		return helperReply{}, errors.Join(ErrDisconnected, err)
	}
	select {
	case result := <-sent:
		if result.err != nil {
			return fail(result.err)
		}
		if result.reply.Fatal {
			return fail(errors.New(result.reply.Error))
		}
		if result.reply.Uncertain {
			return result.reply, ErrWriteUncertain
		}
		if result.reply.Error != "" {
			return result.reply, errors.New(result.reply.Error)
		}
		return result.reply, nil
	case <-ctx.Done():
		return fail(ctx.Err())
	}
}

func (t *helperTransport) Get(ctx context.Context, name string) (float64, error) {
	reply, err := t.exchange(ctx, &helperRequest{Op: "get", Name: name}, nil)
	if err != nil {
		return 0, err
	}
	// A missing value is a protocol error because zero can be valid telemetry.
	if reply.Value == nil {
		return 0, errors.New("SimConnect helper returned no value")
	}
	return *reply.Value, nil
}

func (t *helperTransport) Set(ctx context.Context, name string, value float64) error {
	return t.setChecked(ctx, name, value, nil)
}

func (t *helperTransport) setChecked(ctx context.Context, name string, value float64, check func() error) error {
	_, err := t.exchange(ctx, &helperRequest{Op: "set", Name: name, Value: value}, check)
	return err
}

func (t *helperTransport) Close() error {
	// Wait for the active exchange before closing the shared pipes.
	t.gate <- struct{}{}
	defer func() { <-t.gate }()
	t.stop()
	return nil
}

func (t *helperTransport) stop() {
	if t.cmd == nil {
		return
	}
	cmd := t.cmd
	t.cmd = nil
	// Stop the helper before waiting since the pipe exchange may have stalled.
	_ = cmd.Process.Kill()
	_ = t.in.Close()
	_ = t.out.Close()
	_ = cmd.Wait()
}
