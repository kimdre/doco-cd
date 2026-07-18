package stages

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/commitstatus"
)

func TestSuccessfulCommitStatusDescription(t *testing.T) {
	start := time.Date(2026, time.July, 18, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		startedAt  time.Time
		finishedAt time.Time
		want       string
	}{
		{
			name:       "missing timestamps",
			startedAt:  time.Time{},
			finishedAt: time.Time{},
			want:       "Successful",
		},
		{
			name:       "sub second duration",
			startedAt:  start,
			finishedAt: start.Add(500 * time.Millisecond),
			want:       "Successful in <1s",
		},
		{
			name:       "whole seconds",
			startedAt:  start,
			finishedAt: start.Add(47 * time.Second),
			want:       "Successful in 47s",
		},
		{
			name:       "multi unit duration",
			startedAt:  start,
			finishedAt: start.Add(time.Minute + 2*time.Second),
			want:       "Successful in 1m2s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := successfulCommitStatusDescription(tt.startedAt, tt.finishedAt)
			if got != tt.want {
				t.Fatalf("successfulCommitStatusDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShouldPostPendingCommitStatus(t *testing.T) {
	tests := []struct {
		name           string
		stageName      StageName
		destroyEnabled bool
		pendingPosted  bool
		want           bool
	}{
		{
			name:      "posts after init",
			stageName: StageInit,
			want:      true,
		},
		{
			name:      "does not post after pre deploy",
			stageName: StagePreDeploy,
			want:      false,
		},
		{
			name:           "does not post for destroy",
			stageName:      StageInit,
			destroyEnabled: true,
			want:           false,
		},
		{
			name:          "does not repost pending",
			stageName:     StageInit,
			pendingPosted: true,
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldPostPendingCommitStatus(tt.stageName, tt.destroyEnabled, tt.pendingPosted)
			if got != tt.want {
				t.Fatalf("shouldPostPendingCommitStatus() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestFailureCommitStatusDescription(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "Failed",
		},
		{
			name: "single line error",
			err:  errors.New("compose validation failed"),
			want: "compose validation failed",
		},
		{
			name: "normalizes whitespace",
			err:  errors.New("error from registry:\n denied"),
			want: "error from registry: denied",
		},
		{
			name: "truncates long descriptions",
			err:  errors.New(strings.Repeat("x", 200)),
			want: strings.Repeat("x", 137) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := failureCommitStatusDescription(tt.err)
			if got != tt.want {
				t.Fatalf("failureCommitStatusDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShouldPostFailureCommitStatus(t *testing.T) {
	tests := []struct {
		name           string
		destroyEnabled bool
		want           bool
	}{
		{
			name: "posts for init failures",
			want: true,
		},
		{
			name: "posts for pre deploy failures",
			want: true,
		},
		{
			name: "posts for deploy failures",
			want: true,
		},
		{
			name:           "does not post for destroy",
			destroyEnabled: true,
			want:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldPostFailureCommitStatus(tt.destroyEnabled)
			if got != tt.want {
				t.Fatalf("shouldPostFailureCommitStatus() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestFailureCommitStatusState(t *testing.T) {
	tests := []struct {
		name      string
		stageName StageName
		want      commitstatus.State
	}{
		{
			name:      "init uses error",
			stageName: StageInit,
			want:      commitstatus.StateError,
		},
		{
			name:      "pre deploy uses error",
			stageName: StagePreDeploy,
			want:      commitstatus.StateError,
		},
		{
			name:      "deploy uses failure",
			stageName: StageDeploy,
			want:      commitstatus.StateFailure,
		},
		{
			name:      "cleanup uses failure",
			stageName: StageCleanup,
			want:      commitstatus.StateFailure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := failureCommitStatusState(tt.stageName)
			if got != tt.want {
				t.Fatalf("failureCommitStatusState() = %q, want %q", got, tt.want)
			}
		})
	}
}
