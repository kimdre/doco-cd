package main

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
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
