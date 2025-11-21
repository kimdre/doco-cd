package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/git"
)

func TestNormalizeRepoURL(t *testing.T) {
	t.Helper()

	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty", "", ""},
		{"strip suffix", "https://github.com/example/repo.git", "https://github.com/example/repo"},
		{"strip credentials", "https://oauth2:token@github.com/example/repo.git", "https://github.com/example/repo"},
		{"normalize case", "HTTPS://GITHUB.COM/Example/Repo/", "https://github.com/example/repo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRepoURL(tc.input); got != tc.expected {
				t.Fatalf("expected %s, got %s", tc.expected, got)
			}
		})
	}
}

func TestSelectInlinePollConfigs(t *testing.T) {
	pollBase := config.PollConfig{
		CloneUrl:    config.HttpUrl("https://github.com/example/repo.git"),
		Reference:   git.MainBranch,
		Interval:    10,
		Deployments: []*config.DeployConfig{{Name: "inline", ComposeFiles: []string{"compose.yaml"}}},
	}

	polls := []config.PollConfig{
		pollBase,
		{
			CloneUrl:    config.HttpUrl("https://github.com/example/repo.git"),
			Reference:   git.MainBranch,
			Interval:    10,
			Deployments: []*config.DeployConfig{},
		},
		{
			CloneUrl:     config.HttpUrl("https://github.com/example/repo.git"),
			Reference:    git.MainBranch,
			Interval:     10,
			CustomTarget: "prod",
			Deployments:  []*config.DeployConfig{{Name: "prod", ComposeFiles: []string{"compose.yaml"}}},
		},
	}

	// No custom target should only match poll configs without a custom target
	matches := selectInlinePollConfigs(polls, "https://github.com/example/repo", "")
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}

	// Custom target should match configs with matching target plus defaults
	matches = selectInlinePollConfigs(polls, "https://github.com/example/repo", "prod")
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches when including default + prod, got %d", len(matches))
	}
}

func TestResolveInlineWebhookDeployConfigs(t *testing.T) {
	repoRoot := t.TempDir()

	poll := config.PollConfig{
		CloneUrl:  config.HttpUrl("https://github.com/example/repo.git"),
		Reference: git.MainBranch,
		Interval:  10,
		Deployments: []*config.DeployConfig{
			{
				Name:             "inline-stack",
				WorkingDirectory: ".",
				ComposeFiles:     []string{"compose.yaml"},
			},
		},
	}

	if err := poll.Validate(); err != nil {
		t.Fatalf("validate poll config: %v", err)
	}

	configs, err := resolveInlineWebhookDeployConfigs([]config.PollConfig{poll}, "https://github.com/example/repo", "", repoRoot, "repo", git.MainBranch, ".")
	if err != nil {
		t.Fatalf("resolve inline configs: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	if configs[0].Name != "inline-stack" {
		t.Fatalf("expected inline-stack, got %s", configs[0].Name)
	}
}

func TestResolveInlineWebhookDeployConfigsAutoDiscover(t *testing.T) {
	repoRoot := t.TempDir()
	servicesDir := filepath.Join(repoRoot, "services")
	serviceA := filepath.Join(servicesDir, "service-a")
	serviceB := filepath.Join(servicesDir, "service-b")

	for _, dir := range []string{serviceA, serviceB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}

		composePath := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(composePath, []byte("services:\n  app:\n    image: nginx"), 0o644); err != nil {
			t.Fatalf("write compose: %v", err)
		}
	}

	poll := config.PollConfig{
		CloneUrl:  config.HttpUrl("https://github.com/example/repo.git"),
		Reference: git.MainBranch,
		Interval:  10,
		Deployments: []*config.DeployConfig{
			{
				WorkingDirectory: "services",
				ComposeFiles:     []string{"compose.yaml"},
				AutoDiscover:     true,
			},
		},
	}

	if err := poll.Validate(); err != nil {
		t.Fatalf("validate poll config: %v", err)
	}

	configs, err := resolveInlineWebhookDeployConfigs([]config.PollConfig{poll}, "https://github.com/example/repo", "", repoRoot, "repo", git.MainBranch, ".")
	if err != nil {
		t.Fatalf("resolve inline configs: %v", err)
	}

	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}

	names := map[string]bool{}
	for _, cfg := range configs {
		names[cfg.Name] = true
	}

	if !names["service-a"] || !names["service-b"] {
		t.Fatalf("expected service-a and service-b, got %+v", names)
	}
}
