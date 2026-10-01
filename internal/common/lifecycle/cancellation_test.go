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

func TestIsCanceledExcludesDeadline(t *testing.T) {
	t.Parallel()

	if !IsCanceled(fmt.Errorf("operation: %w", context.Canceled)) {
		t.Fatal("IsCanceled() = false for wrapped cancellation")
	}

	if IsCanceled(context.DeadlineExceeded) {
		t.Fatal("IsCanceled() = true for deadline exceeded")
	}
}

func TestIsTimeout(t *testing.T) {
	t.Parallel()

	cause := errors.New("services not ready")

	timedOut := fmt.Errorf("deploy: %w", MarkTimedOut(cause))
	if !IsTimeout(timedOut) || !IsTimeout(fmt.Errorf("request: %w", context.DeadlineExceeded)) {
		t.Fatal("IsTimeout() = false for a time limit or deadline")
	}

	if timedOut.Error() != "deploy: services not ready" || !errors.Is(timedOut, cause) || MarkTimedOut(nil) != nil {
		t.Fatalf("MarkTimedOut() changed the error: %v", timedOut)
	}

	if IsCancellation(timedOut) {
		t.Fatal("an operation time limit must not be lifecycle cancellation")
	}

	if IsTimeout(context.Canceled) || IsTimeout(errors.New("timeout-like error message")) {
		t.Fatal("IsTimeout() = true for an untyped or canceled error")
	}
}
