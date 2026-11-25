// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"context"
	"slices"
	"testing"
	"time"
)

// operatorTestTransport publishes the follow-up when the service slot is sent
type operatorTestTransport struct {
	page, service, followup Menu
	choices                 []float64
	closeRequests           int
}

func (sim *operatorTestTransport) Get(context.Context, string) (float64, error) {
	// The simulator can still report the menu as open after a close request
	return 1, nil
}

func (sim *operatorTestTransport) Set(_ context.Context, name string, value float64) error {
	switch {
	case name == Remote && value == 1 && len(sim.choices) == 0:
		sim.page = sim.service
	case name == MenuChoice:
		sim.choices = append(sim.choices, value)
		if len(sim.choices) == 1 {
			sim.page = sim.followup
		}
	case name == MenuOpen && value == 0:
		sim.closeRequests++
	}
	return nil
}

func (sim *operatorTestTransport) Close() error { return nil }

func TestExecuteAutoSelectOperator(t *testing.T) {
	tests := []struct {
		name, followup string
		choices        []float64
		closeRequests  int
		polls          int
	}{
		{"annotated GSX choice", "Handling options\nFirst operator\nAutomatic (GSX CHOICE)", []float64{0, 1}, 0, 0},
		{"delayed GSX choice", "Select operator\nFirst operator\nGSX Choice", []float64{0, 1}, 0, 1},
		{"operator menu without GSX choice", "Select operator\nFirst operator\nSecond operator", []float64{0}, 1, 0},
		{"generic title without GSX choice", "Handling options\nFirst operator\nSecond operator", []float64{0}, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sim := &operatorTestTransport{
				service:  ParseMenu("GSX Menu\nRequest boarding"),
				followup: ParseMenu(tt.followup),
			}
			// Keep the service page visible until GSX publishes the next page
			if tt.polls > 0 {
				sim.followup = sim.service
			}
			polls := 0
			session := &Session{
				Sim:      sim,
				ReadMenu: func() Menu { return sim.page },
				Sleep: func(_ context.Context, delay time.Duration) error {
					if delay == 500*time.Millisecond {
						polls++
						if polls == tt.polls {
							sim.page = ParseMenu(tt.followup)
						}
					}
					return nil
				},
			}
			selected, err := session.Execute(context.Background(), "", "boarding", []string{"boarding"}, true)
			if err != nil || selected != "Request boarding" {
				t.Fatalf("Execute = %q, %v", selected, err)
			}
			if !slices.Equal(sim.choices, tt.choices) || sim.closeRequests != tt.closeRequests {
				t.Fatalf("choices = %v, close requests = %d, want %v and %d", sim.choices, sim.closeRequests, tt.choices, tt.closeRequests)
			}
			if polls != tt.polls {
				t.Fatalf("poll waits = %d, want %d", polls, tt.polls)
			}
		})
	}
}
