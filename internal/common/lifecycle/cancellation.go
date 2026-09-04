package lifecycle

import (
	"context"
	"errors"
)

// IsCancellation reports whether err represents canceled or timed-out lifecycle work.
func IsCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
