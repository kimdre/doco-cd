package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/reconciliation"
)

// TestRecoverManagedDeployment_ReloadsConfigFromLocalCheckoutAndRegistersJob verifies that
// recoverManagedDeployment can rebuild reconciliation state for a repository that was already
// deployed in a previous process lifetime: it reloads the deploy config directly from the
// existing local checkout on the data volume (no git clone/fetch, no network) and registers a
// reconciliation job for it.
func TestRecoverManagedDeployment_ReloadsConfigFromLocalCheckoutAndRegistersJob(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()

	repoDirName := "github.com/owner/recover-cmd-test"
	repoDir := filepath.Join(dataDir, repoDirName)

	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repoDir, "compose.yaml"), []byte("services:\n  app:\n    image: alpine:3\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Only a restart-oriented event ("unhealthy") is configured, so registering the job never
	// triggers an actual redeploy of this (non-existent) stack during the test.
	deployConfigYAML := "name: recover-cmd-stack\nreference: main\nreconciliation:\n  enabled: true\n  events:\n    - unhealthy\n"
	if err := os.WriteFile(filepath.Join(repoDir, ".doco-cd.yml"), []byte(deployConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	revision := commitRecoveryTestRepo(t, repoDir, []string{"compose.yaml", ".doco-cd.yml"})

	appConfig := &app.Config{DeployConfigBaseDir: "/"}

	dockerCli, err := docker.CreateDockerCli(true)
	if err != nil {
		t.Fatalf("failed to create docker cli: %v", err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	dataMountPoint := container.MountPoint{Type: "bind", Source: dataDir, Destination: dataDir, Mode: "rw"}

	manager := newTestReconciliationManager(t, reconciliation.Dependencies{
		AppConfig:                appConfig,
		DataMountPoint:           dataMountPoint,
		DockerCLI:                dockerCli,
		MaxConcurrentDeployments: 1,
	})

	ref := docker.ManagedDeploymentRef{
		RepositoryName: "owner/recover-cmd-test",
		RepositoryURL:  "https://github.com/owner/recover-cmd-test.git",
		SourceType:     "git",
		Targets: []docker.ManagedDeploymentTarget{{
			DeploymentName: "recover-cmd-stack",
			ConfigTarget:   "",
			Reference:      "main",
			Revision:       revision.String(),
		}},
	}

	recoverManagedDeployment(t.Context(), appConfig, manager, dataMountPoint, ref, logger.New(logger.LevelCritical).Logger)

	deadline := time.Now().Add(10 * time.Second)

	for {
		if manager.HasJob(repoDirName) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for reconciliation job to be registered for %q", repoDirName)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func commitRecoveryTestRepo(t *testing.T, repoDir string, files []string) plumbing.Hash {
	t.Helper()

	repo, err := gogit.PlainInitWithOptions(repoDir, &gogit.PlainInitOptions{
		DefaultBranch: plumbing.NewBranchReferenceName("main"),
	})
	if err != nil {
		t.Fatalf("initialize recovery test repository: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("open recovery test worktree: %v", err)
	}

	for _, file := range files {
		if _, err = worktree.Add(file); err != nil {
			t.Fatalf("stage recovery test file %q: %v", file, err)
		}
	}

	hash, err := worktree.Commit("recovery fixture", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "doco-cd-test",
			Email: "doco-cd-test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("commit recovery test repository: %v", err)
	}

	return hash
}

func TestReloadManagedDeployConfigs_DedupsAcrossTargets(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(repoDir, ".doco-cd.yml"), []byte("---\nname: base-stack\nreference: main\n---\nname: never-deployed\nreference: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repoDir, ".doco-cd.nas.yml"), []byte("name: nas-stack\nreference: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	appConfig := &app.Config{DeployConfigBaseDir: "/"}

	ref := docker.ManagedDeploymentRef{
		RepositoryName: "owner/repo",
		Targets: []docker.ManagedDeploymentTarget{
			{DeploymentName: "base-stack", ConfigTarget: "", Reference: "main"},
			{DeploymentName: "nas-stack", ConfigTarget: "nas", Reference: "main"},
		},
	}

	configs := reloadManagedDeployConfigs(
		appConfig,
		repoDir,
		repoDir,
		config.SourceTypeGit,
		ref,
		logger.New(logger.LevelCritical).Logger,
	)

	if len(configs) != 2 {
		t.Fatalf("got %d deploy configs, want 2 (base-stack, nas-stack): %#v", len(configs), configs)
	}

	names := map[string]bool{}
	for _, cfg := range configs {
		names[cfg.Name] = true
	}

	if !names["base-stack"] || !names["nas-stack"] {
		t.Fatalf("deploy config names = %#v, want base-stack and nas-stack", names)
	}
}

func TestReloadManagedDeployConfigs_RetainsObservedContextsOnly(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()

	configYAML := `---
name: shared-stack
context: default
reference: main
---
name: shared-stack
context: remote
reference: main
---
name: undeployed-stack
context: remote
reference: main
`
	if err := os.WriteFile(filepath.Join(repoDir, ".doco-cd.yml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	ref := docker.ManagedDeploymentRef{
		RepositoryName: "owner/repo",
		Targets: []docker.ManagedDeploymentTarget{
			{DeploymentName: "shared-stack", Reference: "main", Context: ""},
			{DeploymentName: "shared-stack", Reference: "main", Context: "remote"},
		},
	}

	configs := reloadManagedDeployConfigs(
		&app.Config{DeployConfigBaseDir: "/"},
		repoDir,
		repoDir,
		config.SourceTypeGit,
		ref,
		logger.New(logger.LevelCritical).Logger,
	)
	if len(configs) != 2 {
		t.Fatalf("got %d deploy configs, want one shared-stack config per observed context: %#v", len(configs), configs)
	}

	contexts := map[string]bool{}

	for _, cfg := range configs {
		if cfg.Name != "shared-stack" {
			t.Fatalf("recovered undeployed config %q", cfg.Name)
		}

		contexts[docker.NormalizeContextName(cfg.Context)] = true
	}

	if !contexts[""] || !contexts["remote"] {
		t.Fatalf("recovered contexts = %#v, want default and remote", contexts)
	}
}

func TestReloadManagedDeployConfigs_DoesNotFetchRemoteAutoDiscoveryRepository(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()

	configYAML := `name: remote-stack
repository_url: https://invalid.example/recovery-must-not-fetch.git
reference: main
auto_discovery:
  enabled: true
`
	if err := os.WriteFile(filepath.Join(repoDir, ".doco-cd.yml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	ref := docker.ManagedDeploymentRef{
		RepositoryName: "owner/repo",
		Targets: []docker.ManagedDeploymentTarget{
			{DeploymentName: "remote-stack", Reference: "main"},
		},
	}

	configs := reloadManagedDeployConfigs(
		&app.Config{DeployConfigBaseDir: "/"},
		repoDir,
		repoDir,
		config.SourceTypeGit,
		ref,
		logger.New(logger.LevelCritical).Logger,
	)
	if len(configs) != 0 {
		t.Fatalf("got %d configs, want remote auto-discovery target skipped without fetching", len(configs))
	}
}
