package selfupdate

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetry(t *testing.T) {
	RetryPause = time.Millisecond

	t.Cleanup(func() { RetryPause = time.Second })

	errBoom := errors.New("boom")

	t.Run("succeeds after failures", func(t *testing.T) {
		calls, reported := 0, 0

		err := Retry(context.Background(), func() error {
			calls++
			if calls < RecoveryRetries+1 {
				return errBoom
			}

			return nil
		}, func(attempt int, err error) {
			reported++
			if attempt != reported || !errors.Is(err, errBoom) {
				t.Errorf("onError(%d, %v), want attempt %d", attempt, err, reported)
			}
		})
		if err != nil {
			t.Fatalf("Retry() = %v", err)
		}

		if calls != RecoveryRetries+1 || reported != RecoveryRetries {
			t.Fatalf("calls = %d, reported = %d", calls, reported)
		}
	})

	t.Run("returns the last error", func(t *testing.T) {
		calls := 0

		err := Retry(context.Background(), func() error {
			calls++
			return errBoom
		}, nil)
		if !errors.Is(err, errBoom) {
			t.Fatalf("Retry() = %v, want %v", err, errBoom)
		}

		if calls != RecoveryRetries+1 {
			t.Fatalf("calls = %d, want %d", calls, RecoveryRetries+1)
		}
	})

	t.Run("stops when cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		calls := 0

		err := Retry(ctx, func() error {
			calls++
			return errBoom
		}, nil)
		if !errors.Is(err, errBoom) || !errors.Is(err, context.Canceled) {
			t.Fatalf("Retry() = %v, want both the cause and the cancellation", err)
		}

		if calls != 1 {
			t.Fatalf("calls = %d, want 1", calls)
		}
	})
}
