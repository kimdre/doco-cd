package lifecycle

import (
	"context"
	"errors"
)

// ErrTimedOut marks work that exceeded its own time limit.
// Unlike context.DeadlineExceeded, it is an ordinary failure, not lifecycle cancellation.
var ErrTimedOut = errors.New("timed out")

type timedOutError struct{ error }

func (e timedOutError) Unwrap() []error {
	return []error{e.error, ErrTimedOut}
}

// MarkTimedOut marks err as ErrTimedOut without changing its message.
func MarkTimedOut(err error) error {
	if err == nil {
		return nil
	}

	return timedOutError{err}
}

// IsCancellation reports whether err represents canceled or timed-out lifecycle work.
func IsCancellation(err error) bool {
	return IsCanceled(err) || errors.Is(err, context.DeadlineExceeded)
}

// IsCanceled reports whether err represents explicitly canceled lifecycle work.
func IsCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// IsTimeout reports whether err represents an expired deadline or time limit.
func IsTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimedOut)
}
