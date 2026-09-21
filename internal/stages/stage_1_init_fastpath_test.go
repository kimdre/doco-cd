package stages

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// initFastPathTestRepo creates a local (non-bare) git repository at path with an
// initial commit on "main" and returns its "file://" clone URL.
func initFastPathTestRepo(t *testing.T, path string) string {
	t.Helper()

	repo, err := gogit.PlainInit(path, false)
	if err != nil {
		t.Fatalf("failed to init local test repo: %v", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("failed to set HEAD to main: %v", err)
	}

	filePath := filepath.Join(path, "README.md")
	if err := os.WriteFile(filePath, []byte("initial\n"), 0o600); err != nil {
		t.Fatalf("failed to write README.md: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("failed to add README.md: %v", err)
	}

	if _, err := wt.Commit("initial commit", &gogit.CommitOptions{
		Author: &object.Signature{Name: "fastpath-test", Email: "fastpath-test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	return "file://" + path
}

// resolveViaGitStore mimics what source.Prepare already does for the triggering job: it runs a
// real Resolve+Publish against cloneURL/reference and returns the resulting artifact path,
// revision and mirror directory, so tests can populate RepositoryData as RunInitStage would find
// it after Prepare.
func resolveViaGitStore(t *testing.T, cloneURL, reference, baseDir string) (artifactPath, revision, mirrorDir string) {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	gitStore, err := store.NewGitStore(store.GitStoreOptions{
		Log:          log,
		CloneURL:     cloneURL,
		BaseDir:      baseDir,
		ProxyOptions: transport.ProxyOptions{},
	})
	if err != nil {
		t.Fatalf("NewGitStore() error = %v", err)
	}

	rev, err := gitStore.Resolve(context.Background(), reference)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	artifact, err := gitStore.Publish(context.Background(), rev)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	return artifact.Path, string(rev), gitStore.MirrorDir()
}

// newFastPathStageManager builds a StageManager whose Repository/DeployConfig/Docker fields are
// wired up the way controlplane.Deploy would leave them after Prepare, so RunInitStage's fast
// path (or its slow-path fallback) can be exercised directly.
func newFastPathStageManager(t *testing.T, repository *RepositoryData, deployConfig *deploy.Config, dataMountDest string) *StageManager {
	t.Helper()

	notifier, err := notification.New(notification.Config{})
	if err != nil {
		t.Fatal(err)
	}

	sm, err := NewStageManager(
		Dependencies{
			AppConfig: &app.Config{},
			Notifier:  notifier,
		},
		RunInput{
			Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
			JobID:        "job-fastpath",
			JobTrigger:   JobTriggerWebhook,
			Repository:   repository,
			Docker:       &Docker{DataMountPoint: container.MountPoint{Source: dataMountDest, Destination: dataMountDest}},
			DeployConfig: deployConfig,
			Metadata:     notification.Metadata{},
		},
	)
	if err != nil {
		t.Fatalf("NewStageManager() error = %v", err)
	}

	return sm
}

// TestRunInitStageFastPathReusesResolvedArtifact verifies that when a stack's deploy config
// tracks the same repository/reference Prepare already resolved for the job, RunInitStage does
// not re-fetch: it succeeds even though the repository URL has since become unreachable, and it
// leaves the already-resolved paths/revision/mirror untouched.
func TestRunInitStageFastPathReusesResolvedArtifact(t *testing.T) {
	t.Parallel()

	dataMount := t.TempDir()
	originPath := filepath.Join(t.TempDir(), "origin")
	cloneURL := initFastPathTestRepo(t, originPath)

	repoName := "fastpath/repo"
	baseDir := filepath.Join(dataMount, repoName)

	artifactPath, revision, mirrorDir := resolveViaGitStore(t, cloneURL, "main", baseDir)

	repository := &RepositoryData{
		Source:            config.SourceTypeGit,
		SourceUrl:         "file:///no/such/repo/that/has/gone/away.git", // must not be reachable if fast path is skipped
		Name:              repoName,
		PathInternal:      artifactPath,
		PathExternal:      artifactPath,
		MirrorDir:         mirrorDir,
		Revision:          revision,
		ResolvedReference: "refs/heads/main",
	}

	deployConfig := deploy.New("app", "main")

	sm := newFastPathStageManager(t, repository, deployConfig, dataMount)

	if err := sm.RunInitStage(context.Background(), sm.Log); err != nil {
		t.Fatalf("RunInitStage() error = %v, want fast path to avoid the unreachable clone URL", err)
	}

	if sm.Repository.PathInternal != artifactPath {
		t.Fatalf("PathInternal = %q, want unchanged %q", sm.Repository.PathInternal, artifactPath)
	}

	if sm.Repository.Revision != revision {
		t.Fatalf("Revision = %q, want unchanged %q", sm.Repository.Revision, revision)
	}

	if sm.Repository.MirrorDir != mirrorDir {
		t.Fatalf("MirrorDir = %q, want unchanged %q", sm.Repository.MirrorDir, mirrorDir)
	}

	if sm.Repository.Git == nil {
		t.Fatal("Git = nil, want an opened repository handle")
	}
}

func TestResolvedReferenceMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		resolved   string
		configured string
		want       bool
	}{
		{name: "exact branch", resolved: "refs/heads/main", configured: "refs/heads/main", want: true},
		{name: "short configured branch", resolved: "refs/heads/main", configured: "main", want: true},
		{name: "different branch", resolved: "refs/heads/main", configured: "feature"},
		{name: "short resolved reference stays ambiguous", resolved: "main", configured: "refs/heads/main"},
		{name: "short configured tag stays ambiguous", resolved: "refs/tags/v1.0.0", configured: "v1.0.0"},
		{name: "exact tag", resolved: "refs/tags/v1.0.0", configured: "refs/tags/v1.0.0", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := resolvedReferenceMatches(tt.resolved, tt.configured); got != tt.want {
				t.Fatalf("resolvedReferenceMatches(%q, %q) = %t, want %t",
					tt.resolved, tt.configured, got, tt.want)
			}
		})
	}
}

// TestRunInitStageFastPathSkippedOnReferenceMismatch verifies that a stack whose deploy config
// names a different reference than the one Prepare resolved falls back to the full
// resolve/publish path (and thus can pick up a revision the fast path's cached data doesn't have).
func TestRunInitStageFastPathSkippedOnReferenceMismatch(t *testing.T) {
	t.Parallel()

	dataMount := t.TempDir()
	originPath := filepath.Join(t.TempDir(), "origin")
	cloneURL := initFastPathTestRepo(t, originPath)

	repoName := "slowpath/repo"
	baseDir := filepath.Join(dataMount, repoName)

	artifactPath, revision, mirrorDir := resolveViaGitStore(t, cloneURL, "main", baseDir)

	repository := &RepositoryData{
		Source:            config.SourceTypeGit,
		SourceUrl:         cloneURL,
		Name:              repoName,
		PathInternal:      artifactPath,
		PathExternal:      artifactPath,
		MirrorDir:         mirrorDir,
		Revision:          revision,
		ResolvedReference: "main",
	}

	// This stack's deploy config tracks "main" too, but ResolvedReference will be forced to
	// mismatch below to prove the slow path still works standalone.
	deployConfig := deploy.New("app", "main")

	repository.ResolvedReference = "some-other-branch"

	sm := newFastPathStageManager(t, repository, deployConfig, dataMount)

	if err := sm.RunInitStage(context.Background(), sm.Log); err != nil {
		t.Fatalf("RunInitStage() error = %v", err)
	}

	if sm.Repository.Revision != revision {
		t.Fatalf("Revision = %q, want re-resolved to %q", sm.Repository.Revision, revision)
	}

	if sm.Repository.Git == nil {
		t.Fatal("Git = nil, want an opened repository handle")
	}

	// The slow path resets PathInternal/PathExternal to the store base directory before
	// republishing into it, so the resulting artifact path differs from the fast-path fixture's.
	if sm.Repository.PathInternal == "" {
		t.Fatal("PathInternal = \"\", want a resolved artifact path")
	}
}

// TestRunInitStageFastPathSkippedForRepositoryUrlOverride verifies that a stack whose deploy
// config names its own RepositoryUrl never takes the fast path, even if its reference happens to
// match ResolvedReference, since it deploys a different repository than the one Prepare resolved.
func TestRunInitStageFastPathSkippedForRepositoryUrlOverride(t *testing.T) {
	t.Parallel()

	dataMount := t.TempDir()
	overrideOriginPath := filepath.Join(t.TempDir(), "override-origin")
	overrideCloneURL := initFastPathTestRepo(t, overrideOriginPath)

	primaryOriginPath := filepath.Join(t.TempDir(), "primary-origin")
	primaryCloneURL := initFastPathTestRepo(t, primaryOriginPath)

	repoName := "primary/repo"
	baseDir := filepath.Join(dataMount, repoName)

	artifactPath, revision, mirrorDir := resolveViaGitStore(t, primaryCloneURL, "main", baseDir)

	repository := &RepositoryData{
		Source:            config.SourceTypeGit,
		SourceUrl:         primaryCloneURL,
		Name:              repoName,
		PathInternal:      artifactPath,
		PathExternal:      artifactPath,
		MirrorDir:         mirrorDir,
		Revision:          revision,
		ResolvedReference: "main",
	}

	deployConfig := deploy.New("app", "main")
	deployConfig.RepositoryUrl = config.GitUrl(overrideCloneURL)

	sm := newFastPathStageManager(t, repository, deployConfig, dataMount)

	if err := sm.RunInitStage(context.Background(), sm.Log); err != nil {
		t.Fatalf("RunInitStage() error = %v", err)
	}

	if sm.Repository.SourceUrl != overrideCloneURL {
		t.Fatalf("SourceUrl = %q, want deployment override %q", sm.Repository.SourceUrl, overrideCloneURL)
	}

	if sm.Repository.Git == nil {
		t.Fatal("Git = nil, want an opened repository handle")
	}
}

// TestRunInitStageFastPathSkippedOnGitDepthOverride verifies that a stack requesting a deeper
// history than Prepare mirrored at falls back to the full resolve/publish path. Prepare only ever
// mirrors at the global clone depth, so reusing its mirror would silently give the stack less
// history than it configured. The clone URL is unreachable, so this only passes if the slow path
// (which re-resolves at the stack's own depth) is actually taken.
func TestRunInitStageFastPathSkippedOnGitDepthOverride(t *testing.T) {
	t.Parallel()

	dataMount := t.TempDir()
	originPath := filepath.Join(t.TempDir(), "origin")
	cloneURL := initFastPathTestRepo(t, originPath)

	repoName := "depthoverride/repo"
	baseDir := filepath.Join(dataMount, repoName)

	artifactPath, revision, mirrorDir := resolveViaGitStore(t, cloneURL, "main", baseDir)

	repository := &RepositoryData{
		Source:            config.SourceTypeGit,
		SourceUrl:         "file:///no/such/repo/that/has/gone/away.git",
		Name:              repoName,
		PathInternal:      artifactPath,
		PathExternal:      artifactPath,
		MirrorDir:         mirrorDir,
		Revision:          revision,
		ResolvedReference: "main",
	}

	// AppConfig.GitCloneDepth is 0 in these tests, so any positive override differs from the
	// depth Prepare used and must disqualify the fast path.
	deployConfig := deploy.New("app", "main")
	deployConfig.GitDepth = 5

	sm := newFastPathStageManager(t, repository, deployConfig, dataMount)

	if err := sm.RunInitStage(context.Background(), sm.Log); err == nil {
		t.Fatal("RunInitStage() error = nil, want the slow path to fail on the unreachable clone URL")
	}
}
