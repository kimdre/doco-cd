package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/logger"
)

func TestHandleErrorPreservesLifecycleCancellation(t *testing.T) {
	t.Parallel()

	err := handleError{msg: "deployment failed", err: context.Canceled, httpStatusCode: http.StatusInternalServerError}
	if !isLifecycleCancellation(err) {
		t.Fatalf("handleError does not preserve cancellation identity: %v", err)
	}
}

// TestRunBackgroundRecoversPanic verifies that a panic in background
// orchestration is logged and contained instead of crashing the process, and
// that shutdown still drains the registration.
func TestRunBackgroundRecoversPanic(t *testing.T) {
	t.Parallel()

	background := newBackgroundWork()
	h := &handlerData{
		backgroundCtx:  t.Context(),
		backgroundWork: background,
		log:            logger.New(logger.LevelCritical),
	}

	if err := h.runBackground(t.Context(), func(context.Context) {
		panic("background boom")
	}); err != nil {
		t.Fatalf("runBackground returned error: %v", err)
	}

	done := make(chan struct{})

	go func() {
		background.CloseAndWait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("CloseAndWait did not return after panicking background work")
	}
}

func waitForDeploymentRunStatus(t *testing.T, tracker *deploymentRunTracker, jobID string, want deploymentRunStatus) deploymentRun {
	t.Helper()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	for {
		run, ok := tracker.Get(jobID)
		if ok && run.Status == want {
			return run
		}

		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("timed out waiting for run %q status %q; last run: %#v", jobID, want, run)
		}
	}
}
