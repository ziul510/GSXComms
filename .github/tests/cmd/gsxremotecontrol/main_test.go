// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ziul510/GSXCommsGo/internal/gsx"
)

func TestIPCKeepsPollingBeforeSimulatorStarts(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	in, client := io.Pipe()
	defer client.Close()
	var out bytes.Buffer
	attempts := make(chan struct{}, 5)
	done := make(chan struct{})
	session := gsx.New(nil, t.TempDir())
	go func() {
		defer close(done)
		defer in.Close()
		ipc(ctx, session, in, &out, func(context.Context, *gsx.Session) {
			select {
			case attempts <- struct{}{}:
			default:
			}
		})
	}()
	// Leave stdin idle to verify the retry timer works without a client request
	for range 5 {
		select {
		case <-attempts:
		case <-done:
			t.Fatal("IPC exited while the simulator was disconnected")
		case <-ctx.Done():
			t.Fatal("IPC stopped retrying SimConnect")
		}
	}
	if _, err := io.WriteString(client, "PING\nSHUTDOWN\n"); err != nil {
		t.Fatal(err)
	}
	// Wait for the IPC goroutine before reading the reply buffer
	<-done
	var ready, ping, shutdown struct {
		Status    string
		Ready     bool
		Connected bool
		Shutdown  bool
	}
	decoder := json.NewDecoder(&out)
	for _, response := range []any{&ready, &ping, &shutdown} {
		if err := decoder.Decode(response); err != nil {
			t.Fatal(err)
		}
	}
	if !ready.Ready || ready.Connected || ping.Status != "ok" || ping.Connected || !shutdown.Shutdown {
		t.Fatalf("unexpected IPC responses: ready=%+v ping=%+v shutdown=%+v", ready, ping, shutdown)
	}
}

type ipcTestTransport struct{}

func (ipcTestTransport) Get(ctx context.Context, _ string) (float64, error) {
	// A healthy fake needs only to respect the same cancellation as a real status request
	return 0, ctx.Err()
}
func (ipcTestTransport) Set(ctx context.Context, _ string, _ float64) error {
	// Accept menu writes unless the request was canceled
	return ctx.Err()
}
func (ipcTestTransport) Close() error { return nil } // The healthy fake has no simulator handle to release

type ipcDroppedTransport struct {
	ipcTestTransport
	closed bool
}

func (*ipcDroppedTransport) Get(context.Context, string) (float64, error) {
	// Force every status read to fail so IPC exercises its reconnect path
	return 0, errors.New("connection lost")
}

func (s *ipcDroppedTransport) Close() error {
	// Record the close so the test can prove IPC released the dead transport
	s.closed = true
	return nil
}

func TestIPCReconnectsAfterEstablishedTransportFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	in, client := io.Pipe()
	defer client.Close()
	// Start with an established connection that fails every health poll
	failed := &ipcDroppedTransport{}
	session := gsx.New(failed, t.TempDir())
	session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	var out bytes.Buffer
	reconnected, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer in.Close()
		ipc(ctx, session, in, &out, func(_ context.Context, s *gsx.Session) {
			// Swap in a working fake only after IPC clears the failed transport
			if !s.Connected() {
				s.Sim = ipcTestTransport{}
				close(reconnected)
			}
		})
	}()
	select {
	case <-reconnected:
	case <-done:
		t.Fatal("IPC exited after losing the established transport")
	case <-ctx.Done():
		t.Fatal("IPC did not reconnect")
	}
	if _, err := io.WriteString(client, "PING\nSHUTDOWN\n"); err != nil {
		t.Fatal(err)
	}
	<-done
	var ready, ping map[string]any
	decoder := json.NewDecoder(&out)
	for _, msg := range []*map[string]any{&ready, &ping} {
		if err := decoder.Decode(msg); err != nil {
			t.Fatal(err)
		}
	}
	if !failed.closed || ready["ready"] != true || ping["status"] != "ok" || ping["connected"] != true {
		t.Fatalf("transport closed=%v, ready=%v, ping=%v", failed.closed, ready, ping)
	}
}

func TestIPCReportsMenuReadiness(t *testing.T) {
	for _, tc := range []struct {
		name, menu string
		opened     bool
	}{
		{"no menu", "", false},
		{"active menu", "Ramp\nJetway", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := gsx.New(ipcTestTransport{}, "")
			session.ReadMenu = func() gsx.Menu { return gsx.ParseMenu(tc.menu) }
			session.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
			var out bytes.Buffer
			ipc(t.Context(), session, strings.NewReader("OPEN_MENU\nSHUTDOWN\n"), &out, connect)

			decoder := json.NewDecoder(&out)
			// IPC readiness and menu readiness are separate replies
			var ready struct{ Ready bool }
			var response struct {
				MenuOpened *bool `json:"menu_opened"`
			}
			if err := decoder.Decode(&ready); err != nil || !ready.Ready {
				t.Fatalf("expected ready response: %v", err)
			}
			if err := decoder.Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response.MenuOpened == nil {
				t.Fatal("menu opened response missing")
			}
			if *response.MenuOpened != tc.opened {
				t.Fatalf("menu opened = %v, want %v", *response.MenuOpened, tc.opened)
			}
		})
	}
}
