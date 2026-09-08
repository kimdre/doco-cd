package main

import (
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestResolveWebhookGitCloneURL(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		payload         webhook.ParsedPayload
		rewrites        map[string]string
		expectedURL     string
		expectedApplied bool
	}{
		{
			name: "rewrites by URL prefix",
			payload: webhook.ParsedPayload{
				CloneURL: "https://forgejo.example.com/org/repo.git",
			},
			rewrites: map[string]string{
				"https://forgejo.example.com/": "http://forgejo:3000/",
			},
			expectedURL:     "http://forgejo:3000/org/repo.git",
			expectedApplied: true,
		},
		{
			name: "rewrites by domain host",
			payload: webhook.ParsedPayload{
				CloneURL: "https://forgejo.example.com/org/repo.git",
			},
			rewrites: map[string]string{
				"forgejo.example.com": "forgejo:3000",
			},
			expectedURL:     "https://forgejo:3000/org/repo.git",
			expectedApplied: true,
		},
		{
			name: "rewrites scp URL by URI prefix",
			payload: webhook.ParsedPayload{
				CloneURL: "git@forgejo.example.com:org/repo.git",
			},
			rewrites: map[string]string{
				"git@forgejo.example.com:": "git@forgejo.internal:",
			},
			expectedURL:     "git@forgejo.internal:org/repo.git",
			expectedApplied: true,
		},
		{
			name: "more specific rule wins",
			payload: webhook.ParsedPayload{
				CloneURL: "https://forgejo.example.com/org/repo.git",
			},
			rewrites: map[string]string{
				"forgejo.example.com":          "forgejo:3000",
				"https://forgejo.example.com/": "http://forgejo:3000/",
			},
			expectedURL:     "http://forgejo:3000/org/repo.git",
			expectedApplied: true,
		},
		{
			name: "no rewrite keeps original clone url",
			payload: webhook.ParsedPayload{
				CloneURL: "https://github.com/org/repo.git",
			},
			rewrites: map[string]string{
				"forgejo.example.com": "forgejo:3000",
			},
			expectedURL:     "https://github.com/org/repo.git",
			expectedApplied: false,
		},
		{
			name: "rewrite to absolute local path is normalized to file URL",
			payload: webhook.ParsedPayload{
				CloneURL: "https://forgejo.example.com/org/repo.git",
			},
			rewrites: map[string]string{
				"https://forgejo.example.com/": "/local-repos/",
			},
			expectedURL:     "file:///local-repos/org/repo.git",
			expectedApplied: true,
		},
		{
			name: "payload absolute local path is normalized when no rewrite",
			payload: webhook.ParsedPayload{
				CloneURL: "/local-repos/org/repo.git",
			},
			rewrites:        map[string]string{},
			expectedURL:     "file:///local-repos/org/repo.git",
			expectedApplied: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &app.Config{
				SourceURLRewrites: tc.rewrites,
			}

			url, applied := resolveWebhookGitCloneURL(tc.payload, cfg)

			if url != tc.expectedURL {
				t.Fatalf("expected url %q, got %q", tc.expectedURL, url)
			}

			if applied != tc.expectedApplied {
				t.Fatalf("expected applied=%v, got %v", tc.expectedApplied, applied)
			}
		})
	}
}

func TestRewriteSourceURL_UsedByPollAndWebhook(t *testing.T) {
	t.Parallel()

	cfg := &app.Config{
		SourceURLRewrites: map[string]string{
			"https://forgejo.example.com/": "http://forgejo:3000/",
		},
	}

	webhookURL, webhookApplied := resolveWebhookGitCloneURL(webhook.ParsedPayload{
		CloneURL: "https://forgejo.example.com/org/repo.git",
	}, cfg)
	if !webhookApplied || webhookURL != "http://forgejo:3000/org/repo.git" {
		t.Fatalf("expected webhook URL rewrite to apply, got applied=%v url=%q", webhookApplied, webhookURL)
	}

	pollURL, pollApplied := rewriteSourceURL("https://forgejo.example.com/org/repo.git", cfg.SourceURLRewrites)
	if !pollApplied || pollURL != "http://forgejo:3000/org/repo.git" {
		t.Fatalf("expected poll URL rewrite to apply, got applied=%v url=%q", pollApplied, pollURL)
	}
}

func TestIsWebhookGitCloneURLAllowed(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		url            string
		rewriteApplied bool
		want           bool
	}{
		{
			name: "allows remote URL from payload",
			url:  "https://forgejo.example.com/org/repo.git",
			want: true,
		},
		{
			name: "rejects local URL from payload",
			url:  "file:///local-repos/org/repo.git",
			want: false,
		},
		{
			name:           "allows local URL from configured rewrite",
			url:            "file:///local-repos/org/repo.git",
			rewriteApplied: true,
			want:           true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isWebhookGitCloneURLAllowed(tc.url, tc.rewriteApplied); got != tc.want {
				t.Errorf("isWebhookGitCloneURLAllowed(%q, %t) = %t, want %t", tc.url, tc.rewriteApplied, got, tc.want)
			}
		})
	}
}

func TestRedactURLUserinfo(t *testing.T) {
	t.Parallel()

	const secret = "sentinel-secret"

	redacted := redactURLUserinfo("https://user:" + secret + "@forgejo.internal/org/repo.git")
	if strings.Contains(redacted, secret) || strings.Contains(redacted, "user@") {
		t.Fatalf("redacted URL exposes userinfo: %q", redacted)
	}

	if redacted != "https://forgejo.internal/org/repo.git" {
		t.Fatalf("redacted URL = %q", redacted)
	}
}

func TestShouldUsePayloadSSHURL(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		overrideApplied bool
		payloadSSHURL   string
		resolved        git.ResolvedAuthConfig
		expected        bool
	}{
		{
			name:            "uses payload ssh url when no rewrite and ssh key exists",
			overrideApplied: false,
			payloadSSHURL:   "git@forgejo.example.com:org/repo.git",
			resolved: git.ResolvedAuthConfig{
				SSHPrivateKey: "private-key",
			},
			expected: true,
		},
		{
			name:            "does not use payload ssh url when rewrite is active",
			overrideApplied: true,
			payloadSSHURL:   "git@forgejo.example.com:org/repo.git",
			resolved: git.ResolvedAuthConfig{
				SSHPrivateKey: "private-key",
			},
			expected: false,
		},
		{
			name:            "does not use payload ssh url when no ssh key exists",
			overrideApplied: false,
			payloadSSHURL:   "git@forgejo.example.com:org/repo.git",
			resolved:        git.ResolvedAuthConfig{},
			expected:        false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			usePayloadSSH := shouldUsePayloadSSHURL(tc.overrideApplied, tc.payloadSSHURL, tc.resolved)
			if usePayloadSSH != tc.expected {
				t.Fatalf("expected %v, got %v", tc.expected, usePayloadSSH)
			}
		})
	}
}

func TestMatchingInlineWebhookDeployments(t *testing.T) {
	t.Parallel()

	deployment := func(name string) *deploy.Config {
		return &deploy.Config{Name: name}
	}

	cfg := &app.Config{
		SourceURLRewrites: map[string]string{
			"https://forgejo.example.com/": "http://forgejo:3000/",
		},
		PollConfig: []poll.Config{
			{
				SourceUrl:   "git@forgejo.example.com:org/repo.git",
				Reference:   "main",
				Deployments: []*deploy.Config{deployment("first")},
			},
			{
				SourceUrl:   "https://forgejo.example.com/org/repo.git",
				Reference:   "refs/heads/main",
				Deployments: []*deploy.Config{deployment("second")},
			},
			{
				SourceUrl:    "https://forgejo.example.com/org/repo.git",
				Reference:    "main",
				CustomTarget: "production",
				Deployments:  []*deploy.Config{deployment("wrong-target")},
			},
			{
				SourceUrl:   "https://forgejo.example.com/org/repo.git",
				Reference:   "develop",
				Deployments: []*deploy.Config{deployment("wrong-ref")},
			},
			{
				SourceUrl:   "https://forgejo.example.com/org/other.git",
				Reference:   "main",
				Deployments: []*deploy.Config{deployment("wrong-repo")},
			},
			{
				Source:      "oci",
				SourceUrl:   "forgejo.example.com/org/repo",
				Reference:   "main",
				Deployments: []*deploy.Config{deployment("wrong-source")},
			},
			{
				SourceUrl: "https://forgejo.example.com/org/repo.git",
				Reference: "main",
			},
		},
	}

	got := matchingInlineWebhookDeployments(cfg, webhook.ParsedPayload{
		CloneURL: "https://forgejo.example.com/org/repo.git",
		Ref:      "refs/heads/main",
	}, "http://forgejo:3000/org/repo.git", "")

	if len(got) != 2 {
		t.Fatalf("expected 2 matching deployments, got %d", len(got))
	}

	if got[0].Name != "first" || got[1].Name != "second" {
		t.Fatalf("unexpected deployment order: %q, %q", got[0].Name, got[1].Name)
	}
}

func TestMatchingInlineWebhookDeploymentsMatchesRewrittenSource(t *testing.T) {
	t.Parallel()

	cfg := &app.Config{
		SourceURLRewrites: map[string]string{
			"https://public.example.com/": "http://git:3000/",
		},
		PollConfig: []poll.Config{{
			SourceUrl:   "https://public.example.com/org/repo.git",
			Reference:   "main",
			Deployments: []*deploy.Config{{Name: "app"}},
		}},
	}

	got := matchingInlineWebhookDeployments(cfg, webhook.ParsedPayload{
		CloneURL: "https://different.example.com/org/repo.git",
		Ref:      "refs/heads/main",
	}, "http://git:3000/org/repo.git", "")

	if len(got) != 1 || got[0].Name != "app" {
		t.Fatalf("expected rewritten source identity to match, got %+v", got)
	}
}

func TestMatchingInlineWebhookDeploymentsRequiresMatchingTarget(t *testing.T) {
	t.Parallel()

	cfg := &app.Config{PollConfig: []poll.Config{{
		SourceUrl:    "https://example.com/org/repo.git",
		Reference:    "main",
		CustomTarget: "production",
		Deployments:  []*deploy.Config{{Name: "app"}},
	}}}
	payload := webhook.ParsedPayload{
		CloneURL: "https://example.com/org/repo.git",
		Ref:      "refs/heads/main",
	}

	if got := matchingInlineWebhookDeployments(cfg, payload, payload.CloneURL, ""); got != nil {
		t.Fatalf("expected target mismatch not to match, got %+v", got)
	}

	if got := matchingInlineWebhookDeployments(cfg, payload, payload.CloneURL, " production "); len(got) != 1 {
		t.Fatalf("expected trimmed target to match, got %+v", got)
	}
}

func TestMatchingInlineWebhookDeploymentsIgnoresUnusedSSHURL(t *testing.T) {
	t.Parallel()

	cfg := &app.Config{PollConfig: []poll.Config{{
		SourceUrl:   "git@example.com:org/configured.git",
		Reference:   "main",
		Deployments: []*deploy.Config{{Name: "configured"}},
	}}}

	got := matchingInlineWebhookDeployments(cfg, webhook.ParsedPayload{
		CloneURL: "https://example.com/org/actual.git",
		SSHUrl:   "git@example.com:org/configured.git",
		Ref:      "refs/heads/main",
	}, "https://example.com/org/actual.git", "")
	if got != nil {
		t.Fatalf("expected unused SSH URL not to influence matching, got %+v", got)
	}
}

func TestReferencesMatchKeepsTagsDistinct(t *testing.T) {
	t.Parallel()

	if !referencesMatch("main", "refs/heads/main") {
		t.Fatal("expected short branch and full branch ref to match")
	}

	if referencesMatch("v1.0.0", "refs/tags/v1.0.0") {
		t.Fatal("expected short branch-like ref and tag ref to remain distinct")
	}
}
