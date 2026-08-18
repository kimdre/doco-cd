package notification

import "errors"

// A failed deployment is reported where it fails, and the same error then
// travels up to the poll or webhook handler, which reports it again - two
// messages, same fault, differing only in title. Marking the error where the
// notification is sent lets the outer layer log and count it as before, and stay
// quiet about what the operator already knows.

// notifiedError marks an error whose failure notification was already sent.
type notifiedError struct {
	err error
}

func (e *notifiedError) Error() string { return e.err.Error() }

func (e *notifiedError) Unwrap() error { return e.err }

// MarkNotified marks err as already reported. A nil error stays nil.
func MarkNotified(err error) error {
	if err == nil {
		return nil
	}

	return &notifiedError{err: err}
}

// WasNotified reports whether a failure notification was already sent for err.
func WasNotified(err error) bool {
	var notified *notifiedError

	return errors.As(err, &notified)
}
