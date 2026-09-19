package stages

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/migration"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// initMigratedStoreForCleanupTest creates a repository directory in the post-Phase-2 store
// layout (a bare mirror at "<repoDir>/mirror" plus its lock marker), mirroring
// internal/migration's own test fixture.
func initMigratedStoreForCleanupTest(t *testing.T, repoDir string) {
	t.Helper()

	mirrorDir := filepath.Join(repoDir, store.MirrorSubdir)

	if err := os.MkdirAll(mirrorDir, 0o755); err != nil {
		t.Fatalf("create mirror dir: %v", err)
	}

	if _, err := git.PlainInit(mirrorDir, true); err != nil {
		t.Fatalf("init bare mirror: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repoDir, store.MirrorSubdir+".lock"), nil, 0o600); err != nil {
		t.Fatalf("create mirror lock marker: %v", err)
	}
}

func newCleanupStageManager(t *testing.T, dataMountDestination, repoName string, contexts *docker.ContextRegistry, tracker *migration.LeftoverTracker) *StageManager {
	t.Helper()

	return &StageManager{
		Log: slog.Default(),
		Docker: &Docker{
			DataMountPoint: container.MountPoint{
				Source:      dataMountDestination,
				Destination: dataMountDestination,
			},
		},
		Repository: &RepositoryData{
			Name: repoName,
		},
		Contexts:        contexts,
		LeftoverTracker: tracker,
		Stages: &Stages{
			Cleanup: &CleanupStageData{MetaData: NewMetaData(StageCleanup)},
		},
	}
}

// TestRunCleanupStage_PreservesLeftoversWhenReferenceCheckFails verifies that Stage 5 leaves
// legacy files untouched when Docker context discovery cannot prove they are unreferenced.
func TestRunCleanupStage_PreservesLeftoversWhenReferenceCheckFails(t *testing.T) {
	dataDir := t.TempDir()
	repoName := filepath.Join("github.com", "owner", "repo")
	repoDir := filepath.Join(dataDir, repoName)

	initMigratedStoreForCleanupTest(t, repoDir)

	leftover := filepath.Join(repoDir, "docker-compose.yml")
	if err := os.WriteFile(leftover, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatalf("write legacy leftover: %v", err)
	}

	contexts := docker.NewContextRegistry(nil, docker.ContextRegistryOptions{})

	s := newCleanupStageManager(t, dataDir, repoName, contexts, migration.NewLeftoverTracker())

	if err := s.RunCleanupStage(t.Context(), slog.Default()); err != nil {
		t.Fatalf("RunCleanupStage() error = %v", err)
	}

	if s.Stages.Cleanup.StartedAt.IsZero() || s.Stages.Cleanup.FinishedAt.IsZero() {
		t.Error("expected RunCleanupStage to record stage timestamps")
	}

	if _, err := os.Stat(leftover); err != nil {
		t.Fatalf("expected leftover to survive an inconclusive reference check: %v", err)
	}
}

// TestRunCleanupStage_SwallowsCleanupErrors ensures a failing legacy-leftover check (a docker
// context registry that cannot be resolved) is logged as a warning and never fails the stage -
// cleanup must never turn an already-successful deploy/destroy into a reported failure.
func TestRunCleanupStage_SwallowsCleanupErrors(t *testing.T) {
	dataDir := t.TempDir()
	repoName := filepath.Join("github.com", "owner", "repo")
	repoDir := filepath.Join(dataDir, repoName)

	initMigratedStoreForCleanupTest(t, repoDir)

	if err := os.WriteFile(filepath.Join(repoDir, "docker-compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatalf("write legacy leftover: %v", err)
	}

	// A nil base CLI makes the registry's default-context resolution fail with an error,
	// exercising the error path inside migration.CleanupRepoLeftovers.
	contexts := docker.NewContextRegistry(nil, docker.ContextRegistryOptions{})

	s := newCleanupStageManager(t, dataDir, repoName, contexts, migration.NewLeftoverTracker())

	if err := s.RunCleanupStage(t.Context(), slog.Default()); err != nil {
		t.Fatalf("RunCleanupStage() must never return an error for a cleanup failure, got: %v", err)
	}
}

// TestRunCleanupStage_NoContextsIsANoOp covers the case where no context registry is wired in
// (e.g. a test harness or a deployment path that never set Contexts): the stage must return
// successfully without panicking.
func TestRunCleanupStage_NoContextsIsANoOp(t *testing.T) {
	s := &StageManager{
		Docker:     &Docker{},
		Repository: &RepositoryData{Name: "owner/repo"},
		Stages: &Stages{
			Cleanup: &CleanupStageData{MetaData: NewMetaData(StageCleanup)},
		},
	}

	if err := s.RunCleanupStage(t.Context(), slog.Default()); err != nil {
		t.Fatalf("RunCleanupStage() error = %v", err)
	}
}

// TestRunCleanupStage_SkipsOCISources verifies that Stage 5 never invokes the Git-only legacy
// checkout cleanup for OCI stores.
func TestRunCleanupStage_SkipsOCISources(t *testing.T) {
	var logs bytes.Buffer

	log := slog.New(slog.NewTextHandler(&logs, nil))

	dataDir := t.TempDir()
	repoName := filepath.Join("ghcr.io", "owner", "artifact")
	repoDir := filepath.Join(dataDir, repoName)

	initMigratedStoreForCleanupTest(t, repoDir)

	if err := os.WriteFile(filepath.Join(repoDir, "docker-compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatalf("write sentinel file: %v", err)
	}

	s := newCleanupStageManager(
		t,
		dataDir,
		repoName,
		docker.NewContextRegistry(nil, docker.ContextRegistryOptions{}),
		migration.NewLeftoverTracker(),
	)
	s.Repository.Source = config.SourceTypeOCI

	if err := s.RunCleanupStage(t.Context(), log); err != nil {
		t.Fatalf("RunCleanupStage() error = %v", err)
	}

	if logs.Len() != 0 {
		t.Fatalf("OCI cleanup emitted unexpected log output: %s", logs.String())
	}

	if _, err := os.Stat(filepath.Join(repoDir, "docker-compose.yml")); err != nil {
		t.Fatalf("OCI sentinel file was unexpectedly removed: %v", err)
	}
}
