package stages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/notification"
)

func TestHandleStageFailurePostsNativeTimeoutConclusion(t *testing.T) {
	t.Parallel()

	var conclusion string

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected update of existing check, got %s", r.Method)
		}

		var body struct {
			Conclusion string `json:"conclusion"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode check: %v", err)
		}

		conclusion = body.Conclusion
	}))
	defer server.Close()

	sm := newFailureTestManager(t, "app-native-timeout")
	sm.AppConfig = &app.Config{
		GitCommitStatus: true, GitScmProvider: "github", GitScmApiUrl: config.HttpUrl(server.URL), GitAccessToken: "test-token",
	}
	sm.Repository.SourceUrl = "https://github.com/owner/repo"
	sm.Repository.Revision = "deadbeef"
	sm.commitStatusTarget = &commitstatus.Target{Backend: commitstatus.BackendChecks, CheckRunID: 123, ExternalID: "attempt"}

	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	err := sm.handleStageFailure(ctx, StageDeploy, sm.Log, context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) || conclusion != "timed_out" {
		t.Fatalf("timeout result=%q err=%v", conclusion, err)
	}
}

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
