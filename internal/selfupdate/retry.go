package selfupdate

import (
	"context"
	"errors"
	"time"
)

// RecoveryRetries bounds in-process setup, cleanup and rollback retries. The
// applier's restart policy retries beyond that, so it only absorbs brief
// Docker or filesystem hiccups.
const RecoveryRetries = 2

// ApplierMaxRestarts bounds Docker's restarts of a failing applier while the
// predecessor still serves and can recover the handover. The predecessor lifts
// the bound right before it drains, since the applier is then the only process
// left to finish or roll back.
const ApplierMaxRestarts = 3

// RetryPause is the delay between Retry attempts. Tests may shorten it.
var RetryPause = time.Second

// Retry runs fn up to RecoveryRetries+1 times, pausing between attempts.
// onError, if set, is called after every failed attempt with its 1-based
// number. Cancelling ctx stops further attempts and joins ctx.Err() to the last
// error; pass context.WithoutCancel for recovery that must keep going.
func Retry(ctx context.Context, fn func() error, onError func(attempt int, err error)) error {
	var err error

	for attempt := 1; ; attempt++ {
		if err = fn(); err == nil {
			return nil
		}

		if onError != nil {
			onError(attempt, err)
		}

		if attempt > RecoveryRetries {
			return err
		}

		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(RetryPause):
		}
	}
}
