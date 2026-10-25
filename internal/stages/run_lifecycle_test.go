package stages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		GitCommitStatus: true, GitScmProvider: "github", GitScmApiUrl: config.HttpUrl(server.URL),
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

func TestFailureCheckDetails(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		stage     StageName
		admission bool
		timedOut  bool
	}{
		{name: "init", stage: StageInit},
		{name: "pre-deploy", stage: StagePreDeploy},
		{name: "deploy", stage: StageDeploy},
		{name: "admission", stage: StagePreDeploy, admission: true},
		{name: "timeout", stage: StageDeploy, timedOut: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var received struct {
				Conclusion string `json:"conclusion"`
				Output     struct {
					Title   string `json:"title"`
					Summary string `json:"summary"`
				} `json:"output"`
			}

			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch || !strings.HasSuffix(r.URL.Path, "/check-runs/123") {
					t.Errorf("unexpected check update: %s %s", r.Method, r.URL.Path)
				}

				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Errorf("decode check: %v", err)
				}
			}))
			defer server.Close()

			sm := newFailureTestManager(t, "failure-details-"+tt.name)
			sm.AppConfig = &app.Config{
				GitCommitStatus: true, GitScmProvider: "github",
				GitScmApiUrl: config.HttpUrl(server.URL),
			}
			sm.Repository.SourceUrl = "https://github.com/owner/repo"
			sm.Repository.Revision = "deadbeef"
			sm.commitStatusTarget = &commitstatus.Target{
				Backend: commitstatus.BackendChecks, CheckRunID: 123, ExternalID: "attempt",
			}

			failure := errors.New(strings.Repeat("deployment context ", 15) + "\nimportant error tail")
			wantConclusion := "failure"

			if tt.timedOut {
				failure = fmt.Errorf("%w: %w", failure, context.DeadlineExceeded)
				wantConclusion = "timed_out"
			}

			var err error
			if tt.admission {
				err = sm.runStageOrder(t.Context(), func(context.Context) (func(), error) {
					return nil, failure
				}, StageOrder{
					Order: []StageName{StagePreDeploy},
					Funcs: map[StageName]StageFunc{StagePreDeploy: func(context.Context, *slog.Logger) error { return nil }},
				})
			} else {
				err = sm.handleStageFailure(t.Context(), tt.stage, sm.Log, failure)
			}

			if !errors.Is(err, failure) || received.Conclusion != wantConclusion ||
				received.Output.Title != commitstatus.FailureDescription(failure) ||
				received.Output.Summary != "### Failure details\n\n    "+strings.ReplaceAll(failure.Error(), "\n", "\n    ") {
				t.Fatalf("failure details were lost: output=%+v err=%v", received, err)
			}
		})
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
