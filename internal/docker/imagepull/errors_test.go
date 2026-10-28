package imagepull

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsTransientError(t *testing.T) {
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
		{name: "wrapped failed commit", err: fmt.Errorf("failed to pull images: %w", errors.New("Failed commit on ref \"layer-sha256:abc\"")), want: true},
		{name: "access denied", err: errors.New("pull access denied for nginx, repository does not exist"), want: false},
		{name: "network", err: errors.New("dial tcp: connection refused"), want: false},
		{name: "missing image", err: errors.New("No such image: nginx:latest"), want: false},
		{name: "canceled", err: context.Canceled, want: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := IsTransientError(tc.err); got != tc.want {
				t.Errorf("IsTransientError() = %v, want %v", got, tc.want)
			}
		})
	}
}
