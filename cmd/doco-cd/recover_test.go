package main

import (
	"context"
	"errors"
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

// TestRecoverManagedDeployment_RegistersJobWhenWaitBudgetIsExhausted verifies that exceeding
// the startup wait budget never drops a recovery: the reconciliation job is registered before
// the readiness wait, so an expired context only stops doco-cd from waiting for it.
func TestRecoverManagedDeployment_RegistersJobWhenWaitBudgetIsExhausted(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()

	repoDirName := "github.com/owner/recover-budget-test"
	repoDir := filepath.Join(dataDir, repoDirName)

	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repoDir, "compose.yaml"), []byte("services:\n  app:\n    image: alpine:3\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	deployConfigYAML := "name: recover-budget-stack\nreference: main\nreconciliation:\n  enabled: true\n  events:\n    - unhealthy\n"
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
		RepositoryName: "owner/recover-budget-test",
		RepositoryURL:  "https://github.com/owner/recover-budget-test.git",
		SourceType:     "git",
		Targets: []docker.ManagedDeploymentTarget{{
			DeploymentName: "recover-budget-stack",
			Reference:      "main",
			Revision:       revision.String(),
		}},
	}

	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		recoverManagedDeployment(expired, appConfig, manager, dataMountPoint, ref, logger.New(logger.LevelCritical).Logger)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("recoverManagedDeployment did not return once the wait budget was exhausted")
	}

	deadline := time.Now().Add(10 * time.Second)

	for !manager.HasJob(repoDirName) {
		if time.Now().After(deadline) {
			t.Fatalf("reconciliation job for %q was not registered despite the exhausted wait budget", repoDirName)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func commitRecoveryTestRepo(t *testing.T, repoDir string, files []string) plumbing.Hash {
	t.Helper()

	repo, err := gogit.PlainOpen(repoDir)
	if err != nil {
		if !errors.Is(err, gogit.ErrRepositoryNotExists) {
			t.Fatalf("open recovery test repository: %v", err)
		}

		repo, err = gogit.PlainInitWithOptions(repoDir, &gogit.PlainInitOptions{
			DefaultBranch: plumbing.NewBranchReferenceName("main"),
		})
		if err != nil {
			t.Fatalf("initialize recovery test repository: %v", err)
		}
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
		docker.ManagedSource{Name: "owner/repo", Path: repoDir, Type: config.SourceTypeGit},
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
		docker.ManagedSource{Name: "owner/repo", Path: repoDir, Type: config.SourceTypeGit},
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
		docker.ManagedSource{Name: "owner/repo", Path: repoDir, Type: config.SourceTypeGit},
		ref,
		logger.New(logger.LevelCritical).Logger,
	)
	if len(configs) != 0 {
		t.Fatalf("got %d configs, want remote auto-discovery target skipped without fetching", len(configs))
	}
}

// TestReloadManagedDeployConfigs_ConfigHashMatchSurvivesUnrelatedHeadAdvance proves the fix for
// monorepos with many independently deployed targets sharing one git checkout: a target whose
// recorded config hash still matches its reloaded config is kept even though its recorded
// revision is no longer the checkout's current HEAD (advanced by an unrelated commit, e.g. from
// another target being deployed later).
func TestReloadManagedDeployConfigs_ConfigHashMatchSurvivesUnrelatedHeadAdvance(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(repoDir, "compose.yaml"), []byte("services:\n  app:\n    image: alpine:3\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repoDir, ".doco-cd.yml"), []byte("name: nas-stack\nreference: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	deployedRevision := commitRecoveryTestRepo(t, repoDir, []string{"compose.yaml", ".doco-cd.yml"})

	appConfig := &app.Config{DeployConfigBaseDir: "/"}
	source := docker.ManagedSource{Name: "owner/repo", Path: repoDir, Type: config.SourceTypeGit}

	// Reload once at the deployed revision to obtain the config hash exactly as it would have
	// been recorded on the container's label at deploy time.
	deployedConfigs := reloadManagedDeployConfigs(
		appConfig,
		repoDir,
		source,
		docker.ManagedDeploymentRef{
			RepositoryName: "owner/repo",
			Targets: []docker.ManagedDeploymentTarget{
				{DeploymentName: "nas-stack", Reference: "main", Revision: deployedRevision.String()},
			},
		},
		logger.New(logger.LevelCritical).Logger,
	)
	if len(deployedConfigs) != 1 {
		t.Fatalf("got %d deploy configs at deployed revision, want 1", len(deployedConfigs))
	}

	deployedHash := deployedConfigs[0].Internal.Hash
	if deployedHash == "" {
		t.Fatal("deployed config hash is empty")
	}

	// Advance HEAD with an unrelated commit that does not touch this target's own files,
	// simulating another target being deployed later in the same monorepo checkout.
	if err := os.WriteFile(filepath.Join(repoDir, "unrelated.txt"), []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	commitRecoveryTestRepo(t, repoDir, []string{"unrelated.txt"})

	ref := docker.ManagedDeploymentRef{
		RepositoryName: "owner/repo",
		Targets: []docker.ManagedDeploymentTarget{
			{
				DeploymentName: "nas-stack",
				Reference:      "main",
				Revision:       deployedRevision.String(), // stale: no longer HEAD
				ConfigHash:     deployedHash,
			},
		},
	}

	configs := reloadManagedDeployConfigs(appConfig, repoDir, source, ref, logger.New(logger.LevelCritical).Logger)
	if len(configs) != 1 {
		t.Fatalf("got %d deploy configs, want 1 (matching config hash despite stale revision)", len(configs))
	}
}

// TestReloadManagedDeployConfigs_SkipsWhenConfigHashDiffersFromDeployedState proves that real
// drift (the target's own config genuinely changed since it was deployed) is still detected and
// skipped, even though the config hash check no longer depends on the repository-wide HEAD.
func TestReloadManagedDeployConfigs_SkipsWhenConfigHashDiffersFromDeployedState(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(repoDir, "compose.yaml"), []byte("services:\n  app:\n    image: alpine:3\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repoDir, ".doco-cd.yml"), []byte("name: nas-stack\nreference: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	deployedRevision := commitRecoveryTestRepo(t, repoDir, []string{"compose.yaml", ".doco-cd.yml"})

	// Change the target's own config after it was "deployed", simulating genuine drift.
	if err := os.WriteFile(filepath.Join(repoDir, ".doco-cd.yml"), []byte("name: nas-stack\nreference: main\nworking_dir: changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	commitRecoveryTestRepo(t, repoDir, []string{".doco-cd.yml"})

	ref := docker.ManagedDeploymentRef{
		RepositoryName: "owner/repo",
		Targets: []docker.ManagedDeploymentTarget{
			{
				DeploymentName: "nas-stack",
				Reference:      "main",
				Revision:       deployedRevision.String(),
				ConfigHash:     "sha256-of-config-as-it-was-when-deployed",
			},
		},
	}

	configs := reloadManagedDeployConfigs(
		&app.Config{DeployConfigBaseDir: "/"},
		repoDir,
		docker.ManagedSource{Name: "owner/repo", Path: repoDir, Type: config.SourceTypeGit},
		ref,
		logger.New(logger.LevelCritical).Logger,
	)
	if len(configs) != 0 {
		t.Fatalf("got %d deploy configs, want 0 (config hash mismatch must be skipped)", len(configs))
	}
}
