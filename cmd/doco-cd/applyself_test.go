package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/stages"
)

func TestParseApplySelfArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		args          []string
		wantBootstrap bool
		wantID        string
		wantErr       bool
	}{
		{name: "bootstrap", args: []string{"--bootstrap"}, wantBootstrap: true},
		{name: "journal id", args: []string{"run-1"}, wantID: "run-1"},
		{name: "no arguments", wantErr: true},
		{name: "too many arguments", args: []string{"--bootstrap", "run-1"}, wantErr: true},
		{name: "empty argument", args: []string{""}, wantErr: true},
		{name: "unknown flag", args: []string{"--nope"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			bootstrap, id, err := parseApplySelfArgs(tt.args)

			if tt.wantErr {
				if !errors.Is(err, ErrApplySelfUsage) {
					t.Errorf("err = %v, want ErrApplySelfUsage", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if bootstrap != tt.wantBootstrap {
				t.Errorf("bootstrap = %v, want %v", bootstrap, tt.wantBootstrap)
			}

			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
		})
	}
}

func TestLogBootstrapFailure(t *testing.T) {
	t.Parallel()

	nextOpen := time.Date(2026, time.January, 5, 8, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		err       error
		wantLevel string
		wantMsg   string
		wantAttrs []string
		notWant   []string
	}{
		{
			name:      "sync window deferral",
			err:       fmt.Errorf("deploy: %w", &stages.SyncWindowBlockedError{Stacks: []string{"web"}, Windows: []string{"freeze"}, NextOpen: nextOpen}),
			wantLevel: "WARN",
			wantMsg:   "bootstrap deployment deferred by sync window",
			wantAttrs: []string{"sync_windows=[freeze]", "stacks=[web]", "next_open=2026-01-05T08:00:00.000Z", "manual_sync"},
		},
		{
			name:      "sync window deferral without next open",
			err:       &stages.SyncWindowBlockedError{Stacks: []string{"web"}, Windows: []string{"freeze"}},
			wantLevel: "WARN",
			wantMsg:   "bootstrap deployment deferred by sync window",
			notWant:   []string{"next_open"},
		},
		{
			name:      "other error",
			err:       errors.New("boom"),
			wantLevel: "ERROR",
			wantMsg:   "bootstrap deployment failed",
			wantAttrs: []string{"boom"},
			notWant:   []string{"sync window"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer

			logBootstrapFailure(slog.New(slog.NewTextHandler(&out, nil)), tt.err)

			got := out.String()
			for _, want := range append([]string{"level=" + tt.wantLevel, tt.wantMsg}, tt.wantAttrs...) {
				if !strings.Contains(got, want) {
					t.Errorf("log output %q does not contain %q", got, want)
				}
			}

			for _, unwanted := range tt.notWant {
				if strings.Contains(got, unwanted) {
					t.Errorf("log output %q unexpectedly contains %q", got, unwanted)
				}
			}
		})
	}
}
