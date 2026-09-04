package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsCancellation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "canceled", err: context.Canceled, want: true},
		{name: "wrapped canceled", err: fmt.Errorf("operation: %w", context.Canceled), want: true},
		{name: "deadline exceeded", err: context.DeadlineExceeded, want: true},
		{name: "other error", err: errors.New("failed"), want: false},
		{name: "nil", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCancellation(tt.err); got != tt.want {
				t.Fatalf("IsCancellation(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}
