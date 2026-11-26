package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/reconciliation"
	"github.com/kimdre/doco-cd/internal/selfupdate"
	"github.com/kimdre/doco-cd/internal/webhook"
)

type handoverNotifier struct {
	mu   sync.Mutex
	sent []notification.Level
}

func (n *handoverNotifier) Send(level notification.Level, _, _ string, _ notification.Metadata, _ ...notification.SendOption) error {
	n.mu.Lock()
	n.sent = append(n.sent, level)
	n.mu.Unlock()

	return nil
}

func (n *handoverNotifier) levels() []notification.Level {
	n.mu.Lock()
	defer n.mu.Unlock()

	return append([]notification.Level(nil), n.sent...)
}

func TestHandleEventSelfUpdateHandoverIsNotAFailure(t *testing.T) {
	git.ConfigureAuthResolver(
		[]git.ScopedAuthConfig{{Domains: []string{"127.0.0.1", "github.com"}}},
		"", "", testGitAccessToken, "", git.GitHubAppConfig{},
	)
	t.Cleanup(configureTestGitAuth)

	var statusPosts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		statusPosts.Add(1)

		if r.URL.Path == "/api/v1/version" {
			_, _ = w.Write([]byte(`{"version":"1.25.0"}`))

			return
		}

		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)

	handoverMsg := selfupdate.ErrHandover.Error()

	tests := []struct {
		name        string
		deployErr   error
		wantStatus  controlplane.RunStatus
		wantCode    int
		wantBody    string
		wantFailure bool
	}{
		{
			name:       "handover",
			deployErr:  selfupdate.ErrHandover,
			wantStatus: controlplane.RunStatusSkipped,
			wantCode:   http.StatusAccepted,
			wantBody:   handoverMsg,
		},
		{
			name:       "wrapped handover",
			deployErr:  fmt.Errorf("scale out: %w", selfupdate.ErrHandover),
			wantStatus: controlplane.RunStatusSkipped,
			wantCode:   http.StatusAccepted,
			wantBody:   handoverMsg,
		},
		{
			name:       "joined handover",
			deployErr:  errors.Join(selfupdate.ErrHandover),
			wantStatus: controlplane.RunStatusSkipped,
			wantCode:   http.StatusAccepted,
			wantBody:   handoverMsg,
		},
		{
			name:        "unrelated error",
			deployErr:   errors.New("compose up failed"),
			wantStatus:  controlplane.RunStatusFailed,
			wantCode:    http.StatusInternalServerError,
			wantFailure: true,
		},
		{
			name: "deployment error",
			deployErr: controlplane.DeploymentError{
				Response:       errors.New("deployment failed"),
				Cause:          errors.New("compose up failed"),
				HTTPStatusCode: http.StatusBadGateway,
			},
			wantStatus:  controlplane.RunStatusFailed,
			wantCode:    http.StatusInternalServerError,
			wantFailure: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statusPosts.Store(0)

			notifier := &handoverNotifier{}

			synctest.Test(t, func(t *testing.T) {
				result, rr, err := handleHandoverEvent(t, server.URL, tt.deployErr, notifier)
				if err != nil {
					t.Fatalf("handleEvent error = %v", err)
				}

				synctest.Wait()

				if result.Status == controlplane.RunStatusSucceeded || result.Status != tt.wantStatus {
					t.Fatalf("status = %q, want %q", result.Status, tt.wantStatus)
				}

				if rr.Code != tt.wantCode {
					t.Fatalf("status code = %d, want %d, body = %s", rr.Code, tt.wantCode, rr.Body.String())
				}

				content, errText := decodeWebhookResponse(t, rr)
				if tt.wantBody != "" && (content != tt.wantBody || errText != "") {
					t.Fatalf("body = %s, want content %q", rr.Body.String(), tt.wantBody)
				}

				if tt.wantFailure && errText == "" {
					t.Fatalf("body = %s, want JSON error", rr.Body.String())
				}

				if statusPosts.Load() != 0 {
					t.Fatalf("commit status posts = %d, want 0", statusPosts.Load())
				}

				gotFailure := false

				for _, level := range notifier.levels() {
					if level == notification.Failure {
						gotFailure = true
					}
				}

				if gotFailure != tt.wantFailure {
					t.Fatalf("failure notification = %v, want %v", gotFailure, tt.wantFailure)
				}
			})
		})
	}
}

func decodeWebhookResponse(t *testing.T, rr *httptest.ResponseRecorder) (string, string) {
	t.Helper()

	var resp struct {
		Content string `json:"content"`
		Error   string `json:"error"`
	}

	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	return resp.Content, resp.Error
}

func handleHandoverEvent(t *testing.T, apiURL string, deployErr error, notifier notification.Sender) (controlplane.RunResult, *httptest.ResponseRecorder, error) {
	t.Helper()

	dir := t.TempDir()

	deployment, err := controlplane.NewDeployment(controlplane.DeploymentDependencies{
		SourcePreparer: fakeSourcePreparer{repoName: "owner/repo"},
		Reconciler: fakeReconciler{deploy: func(context.Context, reconciliation.DeployRequest) error {
			return deployErr
		}},
		Contexts: fakeContextResolver{},
		DataMountPoint: container.MountPoint{
			Type:        "bind",
			Source:      dir,
			Destination: dir,
			Mode:        "rw",
		},
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	rr := httptest.NewRecorder()
	result, err := handleEvent(
		t.Context(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		rr,
		&app.Config{
			GitCommitStatus: true,
			GitScmProvider:  string(commitstatus.ProviderGitea),
			GitScmApiUrl:    config.HttpUrl(apiURL),
		},
		webhook.ParsedPayload{
			Source:    webhook.PayloadSourceGit,
			Ref:       "refs/heads/main",
			FullName:  "owner/repo",
			CloneURL:  "https://github.com/owner/repo.git",
			WebURL:    "http://127.0.0.1/owner/repo",
			CommitSHA: plumbing.NewHash("0123456789012345678901234567890123456789"),
		},
		"",
		notification.Metadata{JobID: "job-1", Repository: "owner/repo"},
		"",
		deployment,
		notifier,
	)

	return result, rr, err
}
