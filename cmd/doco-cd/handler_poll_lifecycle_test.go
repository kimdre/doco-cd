package main

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

func pollOutcomeLog(t *testing.T, deployErr error) string {
	t.Helper()

	var logOutput bytes.Buffer

	jobLog := slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reportPollOutcome(jobLog, notification.Metadata{Stack: "doco-cd-updater"}, deployErr, nil, time.Second, "next")

	return logOutput.String()
}

func TestReportPollOutcomeStaysQuietOnShutdown(t *testing.T) {
	t.Parallel()

	output := pollOutcomeLog(t, errors.New("failed to deploy stack: "+context.Canceled.Error()))
	if !strings.Contains(output, "level=ERROR") {
		t.Fatal("unwrapped cancellation text must still be reported as a failure")
	}

	output = pollOutcomeLog(t, context.Canceled)
	if strings.Contains(output, "level=ERROR") || strings.Contains(output, "level=WARN") {
		t.Fatalf("shutdown cancellation must not be reported as a poll failure, got %q", output)
	}

	if !strings.Contains(output, "poll job canceled during application shutdown") {
		t.Fatalf("expected shutdown debug log, got %q", output)
	}
}

func TestReportPollOutcomeReportsGenuineFailures(t *testing.T) {
	t.Parallel()

	output := pollOutcomeLog(t, context.DeadlineExceeded)
	if !strings.Contains(output, "level=ERROR") || !strings.Contains(output, "failed to deploy stack doco-cd-updater") {
		t.Fatalf("deadline exceeded must be reported as a poll failure, got %q", output)
	}

	output = pollOutcomeLog(t, nil)
	if !strings.Contains(output, "job completed successfully") {
		t.Fatalf("expected success log, got %q", output)
	}
}
