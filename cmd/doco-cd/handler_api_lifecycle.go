package main

import (
	"context"
	"errors"

	"github.com/kimdre/doco-cd/internal/graceful"
)

func (h *handlerData) runBackground(requestCtx context.Context, run func(context.Context)) error {
	switch {
	case h.backgroundCtx != nil && h.backgroundWork != nil:
		return h.backgroundWork.Go(func() {
			// Mirror graceful.SafeGo: a panic in background orchestration must
			// be logged instead of crashing the process.
			defer func() {
				if recovered := recover(); recovered != nil {
					logRecoveredPanic(h.log.Logger, "background work", recovered)
				}
			}()

			run(h.backgroundCtx)
		})
	case h.backgroundCtx != nil && h.backgroundWG != nil:
		graceful.SafeGo(h.backgroundWG, h.log.Logger, func() {
			run(h.backgroundCtx)
		})
	default:
		// No lifecycle wiring (tests only): run inline on a detached context so
		// completion stays deterministic for the caller.
		run(context.WithoutCancel(requestCtx))
	}

	return nil
}

func (h *handlerData) runSynchronous(requestCtx context.Context, run func(context.Context) error) error {
	if h.backgroundCtx == nil || h.backgroundWork == nil {
		return run(requestCtx)
	}

	release, err := h.backgroundWork.Register()
	if err != nil {
		return err
	}
	defer release()

	runCtx, cancel := context.WithCancel(requestCtx)
	defer cancel()

	stopApplicationCancel := context.AfterFunc(h.backgroundCtx, cancel) //nolint:contextcheck // The synchronous call is intentionally cancelled by either the request or application lifecycle.
	defer stopApplicationCancel()

	return run(runCtx)
}

func isLifecycleCancellation(err error) bool {
	return errors.Is(err, errBackgroundWorkClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
