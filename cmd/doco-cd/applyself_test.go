package main

import (
	"errors"
	"testing"
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
