package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestPostSkippedWebhookPreservesFailedRetryStatus(t *testing.T) {
	var requests int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++

		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	postSkippedWebhookCommitStatus(t.Context(), &app.Config{
		GitCommitStatus: true,
		GitScmProvider:  string(commitstatus.ProviderGitea),
		GitScmApiUrl:    config.HttpUrl(server.URL),
	}, logger.New(logger.LevelCritical).Logger, webhook.ParsedPayload{
		Source:    webhook.PayloadSourceGit,
		CommitSHA: plumbing.NewHash("0123456789012345678901234567890123456789"),
		FullName:  "owner/repo",
		CloneURL:  "https://git.example.com/owner/repo.git",
	}, "production", &stages.RetrySuppressedError{Stacks: []stages.RetrySuppressedStack{{
		Name: "web", Reason: "budget_exhausted",
	}}})

	if requests != 0 {
		t.Fatalf("retry suppression must preserve the failed status, got %d requests", requests)
	}
}
