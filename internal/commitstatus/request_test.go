package commitstatus_test

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
)

func TestFailureDescription(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "Failed",
		},
		{
			name: "normalizes whitespace",
			err:  simpleError("yaml:\n line 24: could not find expected ':'"),
			want: "yaml: line 24: could not find expected ':'",
		},
		{
			name: "truncates long error",
			err:  simpleError(strings.Repeat("x", 200)),
			want: strings.Repeat("x", 137) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commitstatus.FailureDescription(tt.err)
			if got != tt.want {
				t.Fatalf("FailureDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

type simpleError string

func (e simpleError) Error() string {
	return string(e)
}

func newTestLogger() *slog.Logger {
	return logger.New(logger.LevelCritical).Logger
}

func TestResolveRequest_FallsBackToGitHubAppToken(t *testing.T) {
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

	req, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
		Enabled:          true,
		SourceIsGit:      true,
		SourceURL:        "https://github.com/org/repo.git",
		CommitSHA:        "deadbeef",
		ProviderOverride: "github",
	})
	if !ok {
		t.Fatal("expected ResolveRequest to succeed")
	}

	if req.Token != "ghs-install-token" { // #nosec G101 -- test fixture, not a real credential
		t.Fatalf("expected github app installation token, got '%s'", req.Token)
	}
}

func TestResolveRequest_PrefersExplicitToken(t *testing.T) {
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

	req, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
		Enabled:          true,
		SourceIsGit:      true,
		SourceURL:        "https://github.com/org/repo.git",
		CommitSHA:        "deadbeef",
		ProviderOverride: "github",
		AccessToken:      "pat-token",
	})
	if !ok {
		t.Fatal("expected ResolveRequest to succeed")
	}

	if req.Token != "pat-token" {
		t.Fatalf("expected explicit access token, got '%s'", req.Token)
	}
}

func TestResolveRequest_SkipsWhenNoCredentialsConfigured(t *testing.T) {
	gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	t.Cleanup(func() {
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	_, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
		Enabled:          true,
		SourceIsGit:      true,
		SourceURL:        "https://github.com/org/repo.git",
		CommitSHA:        "deadbeef",
		ProviderOverride: "github",
	})
	if ok {
		t.Fatal("expected ResolveRequest to skip when no credentials are configured")
	}
}

func TestResolveRequest_SkipsWhenDisabled(t *testing.T) {
	_, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
		Enabled:     false,
		SourceIsGit: true,
		SourceURL:   "https://github.com/org/repo.git",
		CommitSHA:   "deadbeef",
		AccessToken: "pat-token",
	})
	if ok {
		t.Fatal("expected ResolveRequest to skip when commit statuses are disabled")
	}
}

func TestResolveRequest_SkipsForNonGitSource(t *testing.T) {
	_, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
		Enabled:     true,
		SourceIsGit: false,
		SourceURL:   "ghcr.io/org/artifact:latest",
		CommitSHA:   "deadbeef",
		AccessToken: "pat-token",
	})
	if ok {
		t.Fatal("expected ResolveRequest to skip for non-git sources")
	}
}

func TestResolveRequest_SkipsWhenNoCommitSHA(t *testing.T) {
	_, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
		Enabled:     true,
		SourceIsGit: true,
		SourceURL:   "https://github.com/org/repo.git",
		AccessToken: "pat-token",
	})
	if ok {
		t.Fatal("expected ResolveRequest to skip when no commit SHA is available")
	}
}

func TestResolveRequest_FallsBackRepoURLAndFullName(t *testing.T) {
	gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	t.Cleanup(func() {
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	req, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
		Enabled:     true,
		SourceIsGit: true,
		SourceURL:   "https://github.com/org/repo.git",
		CommitSHA:   "deadbeef",
		AccessToken: "pat-token",
	})
	if !ok {
		t.Fatal("expected ResolveRequest to succeed")
	}

	if req.RepoURL != "https://github.com/org/repo.git" {
		t.Fatalf("expected repo URL to fall back to source URL, got %q", req.RepoURL)
	}

	if req.RepoFullName != "org/repo" {
		t.Fatalf("expected repo full name derived from URL, got %q", req.RepoFullName)
	}

	if req.Context != commitstatus.DeployContext {
		t.Fatalf("expected context to default to DeployContext, got %q", req.Context)
	}
}
