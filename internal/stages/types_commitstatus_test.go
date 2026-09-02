package stages

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
)

// newTestStageManagerForCommitStatus builds a minimal StageManager sufficient for exercising
// resolveCommitStatusRequest without going through NewStageManager.
func newTestStageManagerForCommitStatus(appConfig *app.Config, repoURL string) *StageManager {
	return &StageManager{
		Log:          slog.Default(),
		AppConfig:    appConfig,
		DeployConfig: &deploy.Config{Name: "stack"},
		Repository: &RepositoryData{
			SourceUrl: repoURL,
			Revision:  "deadbeef",
		},
	}
}

// TestResolveCommitStatusRequest_DelegatesToCommitStatusPackage is a thin wiring test:
// detailed credential-precedence and skip-rule behavior is covered directly against
// commitstatus.ResolveRequest in internal/commitstatus. This only verifies that the
// StageManager wires its own configuration/repository state through correctly.
func TestResolveCommitStatusRequest_DelegatesToCommitStatusPackage(t *testing.T) {
	gitInternal.ConfigureAuthResolver(nil, "", "", "pat-token", "", gitInternal.GitHubAppConfig{})
	t.Cleanup(func() {
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	sm := newTestStageManagerForCommitStatus(&app.Config{
		GitCommitStatus: true,
		GitScmProvider:  "github",
		GitAccessToken:  "pat-token",
	}, "https://github.com/org/repo.git")

	req, ok := sm.resolveCommitStatusRequest()
	if !ok {
		t.Fatal("expected resolveCommitStatusRequest to succeed")
	}

	if req.Token != "pat-token" {
		t.Fatalf("expected explicit access token, got '%s'", req.Token)
	}

	if req.Context != "doco-cd/stack" {
		t.Fatalf("expected context derived from deploy config, got %q", req.Context)
	}
}

func TestPostQueuedCommitStatusUsesDeploymentContext(t *testing.T) {
	gitInternal.ConfigureAuthResolver(nil, "", "", "pat-token", "", gitInternal.GitHubAppConfig{})
	t.Cleanup(func() {
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	var received map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode status request: %v", err)
		}

		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	sm := newTestStageManagerForCommitStatus(&app.Config{
		GitCommitStatus: true,
		GitScmProvider:  "gitea",
		GitScmApiUrl:    config.HttpUrl(server.URL),
		GitAccessToken:  "pat-token",
	}, server.URL+"/org/repo.git")
	sm.JobTrigger = JobTriggerWebhook

	sm.PostQueuedCommitStatus(t.Context())

	if received["state"] != "pending" || received["description"] != "Queued" {
		t.Fatalf("unexpected queued status: %v", received)
	}

	if received["context"] != "doco-cd/stack" {
		t.Fatalf("expected deployment-specific context, got %q", received["context"])
	}
}

func TestResolveCommitStatusRequest_SkipsWhenDisabled(t *testing.T) {
	sm := newTestStageManagerForCommitStatus(&app.Config{
		GitCommitStatus: false,
	}, "https://github.com/org/repo.git")

	_, ok := sm.resolveCommitStatusRequest()
	if ok {
		t.Fatal("expected resolveCommitStatusRequest to skip when commit statuses are disabled")
	}
}

func TestResolveCommitStatusRequest_SkipsForOCISource(t *testing.T) {
	sm := newTestStageManagerForCommitStatus(&app.Config{
		GitCommitStatus: true,
	}, "ghcr.io/org/artifact:latest")
	sm.Repository.Source = "oci"

	_, ok := sm.resolveCommitStatusRequest()
	if ok {
		t.Fatal("expected resolveCommitStatusRequest to skip for OCI sources")
	}
}
