package docker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestIsTransientPullError(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "lost lease", err: errors.New("unable to lease content: lease does not exist: not found"), want: true},
		{name: "wrapped lost lease", err: fmt.Errorf("failed to pull images: %w", errors.New("Unable to lease content: Lease does not exist")), want: true},
		{name: "failed commit", err: errors.New("failed commit on ref \"layer-sha256:abc\": rename: no such file or directory"), want: true},
		{name: "access denied", err: errors.New("pull access denied for nginx, repository does not exist"), want: false},
		{name: "network", err: errors.New("dial tcp: connection refused"), want: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isTransientPullError(tc.err); got != tc.want {
				t.Errorf("isTransientPullError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetryImagePull(t *testing.T) {
	t.Parallel()

	errLease := errors.New("unable to lease content: lease does not exist: not found")
	errDenied := errors.New("pull access denied")

	testCases := []struct {
		name      string
		results   []error
		wantCalls int
		wantErr   error
	}{
		{name: "success", results: []error{nil}, wantCalls: 1},
		{name: "recovers after lost lease", results: []error{errLease, errLease, nil}, wantCalls: 3},
		{name: "gives up after max attempts", results: []error{errLease, errLease, errLease, errLease, errLease}, wantCalls: imagePullAttempts, wantErr: errLease},
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
