package reconciliation

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/stages"
)

func TestQueuedGitHubCheckPrecedesDeploymentAdmission(t *testing.T) {
	git.ConfigureAuthResolver(nil, "", "", "", "", git.GitHubAppConfig{ID: "42", PrivateKey: "test-key"})

	restore := git.SwapGitHubAppTokenProviderForTest(func(string, git.GitHubAppConfig) (string, error) {
		return "installation-token", nil
	})

	t.Cleanup(func() {
		restore()
		git.ConfigureAuthResolver(nil, "", "", "", "", git.GitHubAppConfig{})
	})

	queued := make(chan struct{}, 1)
	running := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
			return
		}

		var body struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode check: %v", err)
		}

		if body.Conclusion != "" {
			t.Errorf("unfinished check has conclusion %q", body.Conclusion)
		}

		switch body.Status {
		case "queued":
			queued <- struct{}{}
		case "in_progress":
			running <- struct{}{}
		default:
			t.Errorf("unexpected check state: %q", body.Status)
		}

		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":123}`))
		}
	}))
	defer server.Close()

	limiter := newTestDeployerLimiter(t, 1)

	releaseHolder, ok := limiter.TryAcquire("holder")
	if !ok {
		t.Fatal("could not hold deployment slot")
	}

	defer func() {
		if releaseHolder != nil {
			releaseHolder()
		}
	}()

	cfg := &app.Config{
		GitCommitStatus: true,
		GitScmProvider:  "github",
		GitScmApiUrl:    config.HttpUrl(server.URL),
	}
	stageMgr := &stages.StageManager{
		Log:          slog.Default(),
		AppConfig:    cfg,
		JobTrigger:   stages.JobTriggerWebhook,
		DeployConfig: &deploy.Config{Name: "stack"},
		Repository:   &stages.RepositoryData{Name: "owner/repo", SourceUrl: "https://github.com/owner/repo", Revision: "deadbeef"},
	}
	manager := &Manager{limiter: limiter}
	done := make(chan error, 1)

	go func() {
		release, err := manager.acquireQueuedDeploymentPhase(t.Context(), stageMgr)
		if err == nil {
			defer release()

			stageMgr.PostCommitStatus(t.Context(), commitstatus.StatePending, "In Progress")
		}

		done <- err
	}()

	select {
	case <-queued:
	case <-time.After(5 * time.Second):
		t.Fatal("queued check not published while waiting for slot")
	}

	select {
	case <-running:
		t.Fatal("check started before deployment admission")
	case err := <-done:
		t.Fatalf("admission completed while slot held: %v", err)
	default:
	}

	releaseHolder()
	releaseHolder = nil

	select {
	case <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("check did not start after deployment admission")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deployment admission did not finish")
	}
}
