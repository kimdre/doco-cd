package commitstatus_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

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
		{
			name: "exact description limit",
			err:  simpleError(strings.Repeat("x", 140)),
			want: strings.Repeat("x", 140),
		},
		{
			name: "Unicode beyond description limit",
			err:  simpleError(strings.Repeat("\u00e4", 141)),
			want: strings.Repeat("\u00e4", 137) + "...",
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

func TestFailureStatus(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		err     error
		details string
		outcome commitstatus.Outcome
	}{
		{name: "nil error", details: "Failed"},
		{
			name: "long error", err: simpleError(strings.Repeat("x", 200) + "important tail"),
			details: strings.Repeat("x", 200) + "important tail",
		},
		{
			name: "multiline literal text", err: simpleError("yaml:\n\t```<details>\n# failure & `reason`\n\nlast line"),
			details: "yaml:\n    \t```<details>\n    # failure & `reason`\n    \n    last line",
		},
		{
			name: "carriage returns", err: simpleError("first\r\n<details>\rlast"),
			details: "first\n    <details>\n    last",
		},
		{
			name: "Unicode", err: simpleError(strings.Repeat("\u00e4", 200) + "\n\u7d42"),
			details: strings.Repeat("\u00e4", 200) + "\n    \u7d42",
		},
		{
			name: "timeout", err: fmt.Errorf("readiness: %w", context.DeadlineExceeded),
			details: "readiness: context deadline exceeded", outcome: commitstatus.OutcomeTimedOut,
		},
		{name: "timeout text", err: simpleError("timeout-like failure"), details: "timeout-like failure"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, state := range []commitstatus.State{commitstatus.StateError, commitstatus.StateFailure} {
				got := commitstatus.FailureStatus(state, tt.err)
				if got.State != state || got.Outcome != tt.outcome ||
					got.Description != commitstatus.FailureDescription(tt.err) ||
					got.Summary != "### Failure details\n\n    "+tt.details {
					t.Fatalf("FailureStatus(%q, %v) = %+v", state, tt.err, got)
				}
			}
		})
	}
}

type simpleError string

func (e simpleError) Error() string {
	return string(e)
}

func TestSuccessDescription(t *testing.T) {
	start := time.Date(2026, time.July, 18, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		startedAt  time.Time
		finishedAt time.Time
		want       string
	}{
		{
			name:       "missing timestamps",
			startedAt:  time.Time{},
			finishedAt: time.Time{},
			want:       "Successful",
		},
		{
			name:       "finished before started",
			startedAt:  start,
			finishedAt: start.Add(-time.Second),
			want:       "Successful",
		},
		{
			name:       "sub second duration",
			startedAt:  start,
			finishedAt: start.Add(500 * time.Millisecond),
			want:       "Successful in <1s",
		},
		{
			name:       "whole seconds",
			startedAt:  start,
			finishedAt: start.Add(47 * time.Second),
			want:       "Successful in 47s",
		},
		{
			name:       "multi unit duration",
			startedAt:  start,
			finishedAt: start.Add(time.Minute + 2*time.Second),
			want:       "Successful in 1m2s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commitstatus.SuccessDescription(tt.startedAt, tt.finishedAt)
			if got != tt.want {
				t.Fatalf("SuccessDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func newTestLogger() *slog.Logger {
	return logger.New(logger.LevelCritical).Logger
}

func TestResolveRequest_FallsBackToGitHubAppToken(t *testing.T) {
	scoped := []gitInternal.ScopedAuthConfig{{Domains: []string{"github.com", "ghe.example"}}}
	gitInternal.ConfigureAuthResolver(scoped, "", "", "", "", gitInternal.GitHubAppConfig{
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

	if req.Target == nil || req.Target.Backend != commitstatus.BackendChecks || req.Target.AppID != "12345" ||
		req.Target.ExternalID == "" {
		t.Fatalf("expected GitHub App check target, got %+v", req.Target)
	}

	legacy := &commitstatus.Target{Backend: commitstatus.BackendStatus}

	pinned, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
		Enabled: true, SourceIsGit: true, SourceURL: "https://github.com/org/repo.git",
		CommitSHA: "deadbeef", Target: legacy,
	})
	if !ok || pinned.Target != legacy || pinned.Target.Backend != commitstatus.BackendStatus {
		t.Fatalf("legacy handover backend changed: %+v", pinned.Target)
	}

	for _, override := range []string{"auto", "gitea"} {
		checks := &commitstatus.Target{Backend: commitstatus.BackendChecks, ExternalID: "doco-cd:handover", CheckRunID: 42}

		pinned, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
			Enabled: true, SourceIsGit: true, SourceURL: "https://ghe.example/org/repo.git",
			CommitSHA: "deadbeef", ProviderOverride: override, Target: checks,
		})
		if !ok || pinned.Target != checks || pinned.Provider != commitstatus.ProviderGitHub {
			t.Fatalf("check handover provider changed with override %q: %+v", override, pinned)
		}
	}

	other, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
		Enabled: true, SourceIsGit: true, SourceURL: "https://github.com/org/repo.git",
		CommitSHA: "deadbeef", ProviderOverride: "gitea",
	})
	if !ok || other.Target.Backend != commitstatus.BackendStatus {
		t.Fatalf("non-GitHub provider switched to checks: %+v", other.Target)
	}
}

func TestResolveRequest_PrefersScopedToken(t *testing.T) {
	scoped := []gitInternal.ScopedAuthConfig{{Domains: []string{"github.com"}, GitAccessToken: "pat-token"}}
	gitInternal.ConfigureAuthResolver(scoped, "", "", "", "", gitInternal.GitHubAppConfig{
		ID:         "12345",
		PrivateKey: "test-private-key",
	})

	restoreProvider := gitInternal.SwapGitHubAppTokenProviderForTest(func(_ string, _ gitInternal.GitHubAppConfig) (string, error) {
		t.Fatal("github app token provider should not be called when a scoped token is set")
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
	})
	if !ok {
		t.Fatal("expected ResolveRequest to succeed")
	}

	if req.Token != "pat-token" {
		t.Fatalf("expected scoped access token, got '%s'", req.Token)
	}

	if req.Target == nil || req.Target.Backend != commitstatus.BackendStatus {
		t.Fatalf("expected token-only legacy status target, got %+v", req.Target)
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

// A global token must not be sent to a reporting host that GIT_AUTH_DOMAINS does not list.
// The status goes to the payload web URL, so a scoped clone URL must not authorize it.
func TestResolveRequest_SkipsUnscopedReportingHost(t *testing.T) {
	scoped := []gitInternal.ScopedAuthConfig{{Domains: []string{"github.com"}}}
	gitInternal.ConfigureAuthResolver(scoped, "", "", "pat-token", "", gitInternal.GitHubAppConfig{
		ID:         "12345",
		PrivateKey: "test-private-key",
	})

	restoreProvider := gitInternal.SwapGitHubAppTokenProviderForTest(func(repoURL string, _ gitInternal.GitHubAppConfig) (string, error) {
		t.Errorf("github app token provider called for unscoped reporting host %s", repoURL)
		return "", nil
	})

	t.Cleanup(func() {
		restoreProvider()
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	tests := []commitstatus.RequestParams{
		{SourceURL: "https://attacker.example/org/repo.git"},
		{SourceURL: "https://github.com/org/repo.git", PayloadWebURL: "https://attacker.example/org/repo"},
		{SourceURL: "git@github.com:org/repo.git", PayloadWebURL: "https://attacker.example/org/repo"},
	}

	for _, params := range tests {
		params.Enabled = true
		params.SourceIsGit = true
		params.CommitSHA = "deadbeef"

		if req, ok := commitstatus.ResolveRequest(newTestLogger(), params); ok {
			t.Fatalf("ResolveRequest(%+v) = %+v, want skip", params, req)
		}
	}

	// The SSH clone URL and web URL point to the same scoped host.
	req, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
		Enabled: true, SourceIsGit: true, CommitSHA: "deadbeef",
		SourceURL: "git@github.com:org/repo.git", PayloadWebURL: "https://github.com/org/repo",
	})
	if !ok || req.Token != "pat-token" {
		t.Fatalf("ResolveRequest() for scoped web URL = %+v, %v, want global token", req, ok)
	}
}

func TestResolveRequest_SkipsWhenDisabled(t *testing.T) {
	_, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
		Enabled:     false,
		SourceIsGit: true,
		SourceURL:   "https://github.com/org/repo.git",
		CommitSHA:   "deadbeef",
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
	})
	if ok {
		t.Fatal("expected ResolveRequest to skip when no commit SHA is available")
	}
}

func TestResolveRequest_FallsBackRepoURLAndFullName(t *testing.T) {
	scoped := []gitInternal.ScopedAuthConfig{{Domains: []string{"github.com"}}}
	gitInternal.ConfigureAuthResolver(scoped, "", "", "pat-token", "", gitInternal.GitHubAppConfig{})
	t.Cleanup(func() {
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	req, ok := commitstatus.ResolveRequest(newTestLogger(), commitstatus.RequestParams{
		Enabled:     true,
		SourceIsGit: true,
		SourceURL:   "https://github.com/org/repo.git",
		CommitSHA:   "deadbeef",
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
