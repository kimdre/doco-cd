package docker

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryImagePull(t *testing.T) {
	t.Parallel()

	errLease := errors.New("unable to lease content: lease does not exist: not found")
	errExtract := errors.New("failed to extract layer: failed to Lchown /snapshot/usr/bin/bao: no such file or directory")
	errPermission := errors.New("failed to extract layer: failed to Lchown /snapshot/usr/bin/bao: permission denied")
	errDenied := errors.New("pull access denied")

	testCases := []struct {
		name      string
		results   []error
		wantCalls int
		wantErr   error
	}{
		{name: "success", results: []error{nil}, wantCalls: 1},
		{name: "recovers after lost lease", results: []error{errLease, errLease, nil}, wantCalls: 3},
		{name: "recovers after layer extraction loses file", results: []error{errExtract, nil}, wantCalls: 2},
		{name: "gives up after max attempts", results: []error{errLease, errLease, errLease, errLease, errLease}, wantCalls: imagePullAttempts, wantErr: errLease},
		{name: "gives up after repeated missing layer files", results: []error{errExtract, errExtract, errExtract, errExtract, nil}, wantCalls: imagePullAttempts, wantErr: errExtract},
		{name: "does not retry extraction permission denied", results: []error{errPermission, nil}, wantCalls: 1, wantErr: errPermission},
		{name: "does not retry other errors", results: []error{errDenied, nil}, wantCalls: 1, wantErr: errDenied},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			calls := 0

			err := retryImagePull(t.Context(), time.Millisecond, func() error {
				result := tc.results[calls]
				calls++

				return result
			})

			if !errors.Is(err, tc.wantErr) {
				t.Errorf("retryImagePull() error = %v, want %v", err, tc.wantErr)
			}

			if calls != tc.wantCalls {
				t.Errorf("pull calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestRetryImagePullStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	calls := 0

	err := retryImagePull(ctx, time.Minute, func() error {
		calls++

		cancel()

		return errors.New("lease does not exist")
	})
	if err == nil {
		t.Fatal("retryImagePull() error = nil, want error")
	}

	if calls != 1 {
		t.Errorf("pull calls = %d, want 1", calls)
	}
}
