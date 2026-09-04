package lifecycle

import (
	"context"
	"errors"
)

// IsCancellation reports whether err represents canceled or timed-out lifecycle work.
func IsCancellation(err error) bool {
	return IsCanceled(err) || errors.Is(err, context.DeadlineExceeded)
}

// IsCanceled reports whether err represents explicitly canceled lifecycle work.
func IsCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}
