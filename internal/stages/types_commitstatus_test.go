package stages

import (
	"log/slog"
	"testing"

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

func TestResolveCommitStatusRequest_FallsBackToGitHubAppToken(t *testing.T) {
	gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{
		ID:         "12345",
		PrivateKey: "test-private-key",
	})

	restoreProvider := gitInternal.SwapGitHubAppTokenProviderForTest(func(_ string, cfg gitInternal.GitHubAppConfig) (string, error) {
		if cfg.ID != "12345" {
			t.Fatalf("expected app id 12345, got %s", cfg.ID)
		}

		return "ghs-install-token", nil // #nosec G101 -- test fixture, not a real credential
	})

	t.Cleanup(func() {
		restoreProvider()
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	sm := newTestStageManagerForCommitStatus(&app.Config{
		GitCommitStatus: true,
		GitScmProvider:  "github",
	}, "https://github.com/org/repo.git")

	_, _, _, _, _, token, _, ok := sm.resolveCommitStatusRequest()
	if !ok {
		t.Fatal("expected resolveCommitStatusRequest to succeed")
	}

	if token != "ghs-install-token" { // #nosec G101 -- test fixture, not a real credential
		t.Fatalf("expected github app installation token, got '%s'", token)
	}
}

func TestResolveCommitStatusRequest_PrefersExplicitToken(t *testing.T) {
	gitInternal.ConfigureAuthResolver(nil, "", "", "pat-token", "", gitInternal.GitHubAppConfig{
		ID:         "12345",
		PrivateKey: "test-private-key",
	})

	restoreProvider := gitInternal.SwapGitHubAppTokenProviderForTest(func(_ string, _ gitInternal.GitHubAppConfig) (string, error) {
		t.Fatal("github app token provider should not be called when an explicit token is set")
		return "", nil
	})

	t.Cleanup(func() {
		restoreProvider()
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	sm := newTestStageManagerForCommitStatus(&app.Config{
		GitCommitStatus: true,
		GitScmProvider:  "github",
		GitAccessToken:  "pat-token",
	}, "https://github.com/org/repo.git")

	_, _, _, _, _, token, _, ok := sm.resolveCommitStatusRequest()
	if !ok {
		t.Fatal("expected resolveCommitStatusRequest to succeed")
	}

	if token != "pat-token" {
		t.Fatalf("expected explicit access token, got '%s'", token)
	}
}

func TestResolveCommitStatusRequest_SkipsWhenNoCredentialsConfigured(t *testing.T) {
	gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	t.Cleanup(func() {
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	sm := newTestStageManagerForCommitStatus(&app.Config{
		GitCommitStatus: true,
		GitScmProvider:  "github",
	}, "https://github.com/org/repo.git")

	_, _, _, _, _, _, _, ok := sm.resolveCommitStatusRequest()
	if ok {
		t.Fatal("expected resolveCommitStatusRequest to skip when no credentials are configured")
	}
}
