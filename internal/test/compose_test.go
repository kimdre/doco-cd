package test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRetryComposeUp(t *testing.T) {
	t.Parallel()

	errLease := errors.New("Error response from daemon: unable to lease content: lease does not exist: not found")
	errCommit := errors.New("failed commit on ref \"layer-sha256:abc\": rename: no such file or directory")
	errExtract := errors.New("failed to extract layer: failed to Lchown /snapshot/usr/bin/bao: no such file or directory")
	errPermission := errors.New("failed to extract layer: failed to Lchown /snapshot/usr/bin/bao: permission denied")
	errDiskFull := errors.New("failed to extract layer: write /snapshot/usr/bin/bao: no space left on device")
	errMissingFile := errors.New("open /tmp/compose.yml: no such file or directory")
	errMissing := errors.New("No such image: nginx:latest")
	errDenied := errors.New("pull access denied for nginx, repository does not exist")
	errNetwork := errors.New("dial tcp: connection refused")
	errConfig := errors.New("invalid compose project")

	testCases := []struct {
		name      string
		results   []error
		wantCalls int
		wantErr   error
	}{
		{name: "success", results: []error{nil}, wantCalls: 1},
		{name: "recovers after lost lease", results: []error{errLease, errLease, nil}, wantCalls: 3},
		{name: "recovers after wrapped lost lease", results: []error{fmt.Errorf("pull images: %w", errLease), nil}, wantCalls: 2},
		{name: "recovers after failed commit", results: []error{errCommit, nil}, wantCalls: 2},
		{name: "recovers after layer extraction loses file", results: []error{errExtract, nil}, wantCalls: 2},
		{name: "recovers after wrapped layer extraction", results: []error{fmt.Errorf("pull images: %w", errExtract), nil}, wantCalls: 2},
		{name: "recovers after missing image", results: []error{errMissing, nil}, wantCalls: 2},
		{name: "gives up after three attempts", results: []error{errLease, errLease, errLease, nil}, wantCalls: 3, wantErr: errLease},
		{name: "gives up after repeated missing layer files", results: []error{errExtract, errExtract, errExtract, nil}, wantCalls: 3, wantErr: errExtract},
		{name: "does not retry extraction permission denied", results: []error{errPermission, nil}, wantCalls: 1, wantErr: errPermission},
		{name: "does not retry extraction disk full", results: []error{errDiskFull, nil}, wantCalls: 1, wantErr: errDiskFull},
		{name: "does not retry unrelated missing files", results: []error{errMissingFile, nil}, wantCalls: 1, wantErr: errMissingFile},
		{name: "does not retry access denied", results: []error{errDenied, nil}, wantCalls: 1, wantErr: errDenied},
		{name: "does not retry network errors", results: []error{errNetwork, nil}, wantCalls: 1, wantErr: errNetwork},
		{name: "does not retry invalid configuration", results: []error{errConfig, nil}, wantCalls: 1, wantErr: errConfig},
		{name: "stops when error becomes permanent", results: []error{errLease, errDenied, nil}, wantCalls: 2, wantErr: errDenied},
		{name: "stops when extraction error becomes permanent", results: []error{errExtract, errPermission, nil}, wantCalls: 2, wantErr: errPermission},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			calls := 0

			err := retryComposeUp(t.Context(), time.Millisecond, func() error {
				result := tc.results[calls]
				calls++

				return result
			})
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("retryComposeUp() error = %v, want %v", err, tc.wantErr)
			}

			if calls != tc.wantCalls {
				t.Errorf("startup calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestRetryComposeUpStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	calls := 0

	err := retryComposeUp(ctx, time.Minute, func() error {
		calls++

		cancel()

		return errors.New("lease does not exist")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("retryComposeUp() error = %v, want context.Canceled", err)
	}

	if calls != 1 {
		t.Errorf("startup calls = %d, want 1", calls)
	}
}
