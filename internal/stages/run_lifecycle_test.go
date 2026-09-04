package stages

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/notification"
)

func TestHandleStageFailureSuppressesLifecycleCancellationReporting(t *testing.T) {
	t.Parallel()

	sm := newFailureTestManager(t, "app-lifecycle-cancellation")

	var logOutput bytes.Buffer

	stageLog := slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))

	err := sm.handleStageFailure(context.Background(), StageDeploy, stageLog, context.Canceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("handleStageFailure() error = %v, want context canceled", err)
	}

	if notification.WasNotified(err) {
		t.Fatal("lifecycle cancellation must not send a failure notification")
	}

	if _, ok := sm.lastDeploymentFailure(); !ok {
		t.Fatal("lifecycle cancellation during deployment must be recorded for retry")
	}

	if !strings.Contains(logOutput.String(), "deployment canceled during application shutdown") {
		t.Fatalf("expected lifecycle cancellation debug log, got %q", logOutput.String())
	}
}

func TestHandleStageFailureReportsOrdinaryFailure(t *testing.T) {
	t.Parallel()

	sm := newFailureTestManager(t, "app-ordinary-failure")
	sent := make(chan notification.Metadata, 1)
	sm.Notifier = recordingNotificationSender{metadata: sent}
	failure := errors.New("deploy failed")

	err := sm.handleStageFailure(context.Background(), StageDeploy, sm.Log, failure)
	if !notification.WasNotified(err) {
		t.Fatal("ordinary failure must send a failure notification")
	}

	if !errors.Is(err, failure) {
		t.Fatalf("handleStageFailure() error = %v, want wrapped %v", err, failure)
	}

	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("ordinary failure notification was not sent")
	}
}

func TestHandleStageFailureReportsDeadlineExceeded(t *testing.T) {
	t.Parallel()

	sm := newFailureTestManager(t, "app-deadline-exceeded")
	sent := make(chan notification.Metadata, 1)
	sm.Notifier = recordingNotificationSender{metadata: sent}

	err := sm.handleStageFailure(context.Background(), StageDeploy, sm.Log, context.DeadlineExceeded)
	if !notification.WasNotified(err) {
		t.Fatal("deadline exceeded must send a failure notification")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handleStageFailure() error = %v, want deadline exceeded", err)
	}

	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("deadline exceeded notification was not sent")
	}
}
