package migration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// migrationTestClient is a minimal client.APIClient stub that only
// implements ContainerList, matching the pattern used by
// internal/docker's own tests for the same interface.
type migrationTestClient struct {
	client.APIClient

	containers []container.Summary
}

func (c *migrationTestClient) ContainerList(_ context.Context, _ client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: c.containers}, nil
}

// initLegacyCheckout creates a non-bare git repository directly at repoDir,
// mirroring the old on-disk layout ("<repoDir>/.git" plus
// working-tree files).
func initLegacyCheckout(t *testing.T, repoDir string) {
	t.Helper()

	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("create repo dir: %v", err)
	}

	if _, err := git.PlainInit(repoDir, false); err != nil {
		t.Fatalf("init legacy checkout: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repoDir, "docker-compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatalf("write working-tree file: %v", err)
	}
}

// initMigratedStore creates a repository directory in the post-Phase-2
// store layout: a bare mirror at "<repoDir>/mirror".
func initMigratedStore(t *testing.T, repoDir string) {
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

func TestRun_BootstrapsLegacyCheckoutToBareMirror(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initLegacyCheckout(t, repoDir)

	apiClient := &migrationTestClient{}

	if err := Run(t.Context(), nil, apiClient, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	mirrorDir := filepath.Join(repoDir, store.MirrorSubdir)

	if _, err := os.Stat(filepath.Join(repoDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("expected legacy .git directory to be gone, stat err = %v", err)
	}

	repo, err := git.PlainOpen(mirrorDir)
	if err != nil {
		t.Fatalf("open migrated mirror: %v", err)
	}

	cfg, err := repo.Config()
	if err != nil {
		t.Fatalf("read migrated mirror config: %v", err)
	}

	if !cfg.Core.IsBare {
		t.Error("expected migrated mirror to be marked bare")
	}

	// The working-tree file is still present: nothing consulted the
	// (empty) container list to say otherwise, so it must not be removed on
	// this same pass that just created the mirror... but since there is no
	// container referencing it, cleanup should have removed it immediately.
	if _, err := os.Stat(filepath.Join(repoDir, "docker-compose.yml")); !os.IsNotExist(err) {
		t.Fatalf("expected unreferenced legacy working-tree file to be cleaned up, stat err = %v", err)
	}
}

func TestRun_KeepsLegacyFilesReferencedByRunningContainer(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initLegacyCheckout(t, repoDir)

	apiClient := &migrationTestClient{
		containers: []container.Summary{
			{
				Labels: map[string]string{
					docker.DocoCDLabels.Deployment.WorkingDir: repoDir,
				},
			},
		},
	}

	if err := Run(t.Context(), nil, apiClient, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// The mirror must still be bootstrapped (never touches the network,
	// unconditional on any container reference)...
	if _, err := os.Stat(filepath.Join(repoDir, store.MirrorSubdir)); err != nil {
		t.Fatalf("expected mirror to be bootstrapped: %v", err)
	}

	// ...but the working-tree file must survive, since a running container
	// still references it directly.
	if _, err := os.Stat(filepath.Join(repoDir, "docker-compose.yml")); err != nil {
		t.Fatalf("expected referenced legacy working-tree file to survive: %v", err)
	}
}

func TestRun_LeavesAlreadyMigratedRepoUntouched(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")
	mirrorDir := filepath.Join(repoDir, store.MirrorSubdir)

	initMigratedStore(t, repoDir)

	marker := filepath.Join(mirrorDir, "marker")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write marker file: %v", err)
	}

	submoduleMarker := filepath.Join(repoDir, store.SubmodulesSubdir, "cached.git", "HEAD")
	if err := os.MkdirAll(filepath.Dir(submoduleMarker), 0o755); err != nil {
		t.Fatalf("create submodule cache: %v", err)
	}

	if err := os.WriteFile(submoduleMarker, []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatalf("write submodule cache marker: %v", err)
	}

	apiClient := &migrationTestClient{}

	if err := Run(t.Context(), nil, apiClient, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected already-migrated repo to be left untouched: %v", err)
	}

	if _, err := os.Stat(submoduleMarker); err != nil {
		t.Fatalf("expected submodule cache to be left untouched: %v", err)
	}
}

func TestRun_LeavesFreshInstallUntouched(t *testing.T) {
	dataDir := t.TempDir()

	apiClient := &migrationTestClient{}

	if err := Run(t.Context(), nil, apiClient, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

// TestRun_PreservesLiveSourceLockFile guards the reason migration must not
// touch "<repoDir>.lock": source.Prepare acquires exactly that path's lock
// on every deployment, for Git and OCI sources alike, so the file is live
// rather than a pre-migration leftover. Removing it while another process
// holds it would let the next acquirer lock a different inode and break
// mutual exclusion outright.
func TestRun_PreservesLiveSourceLockFile(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initLegacyCheckout(t, repoDir)

	unlock := sourcecache.AcquirePathLock(repoDir)
	defer unlock()

	lockFile := repoDir + ".lock"
	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("expected AcquirePathLock to create %s: %v", lockFile, err)
	}

	apiClient := &migrationTestClient{}

	if err := Run(t.Context(), nil, apiClient, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("migration removed a live cross-process lock file: %v", err)
	}
}

func TestRun_MissingDataMountPointIsNotAnError(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "does-not-exist")

	apiClient := &migrationTestClient{}

	if err := Run(t.Context(), nil, apiClient, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

// TestRun_LeavesPublishedArtifactContentUntouched guards against migration
// mistaking a deployed repository's own directory for a store root. The walk
// descends into published artifacts, and repository content is free to
// contain an ordinary directory called "mirror"; if that were accepted as a
// store root, everything published next to it would be deleted as "legacy
// working-tree files".
func TestRun_LeavesPublishedArtifactContentUntouched(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initMigratedStore(t, repoDir)

	artifactDir := filepath.Join(repoDir, store.ArtifactsSubdir, "abc123")
	if err := os.MkdirAll(filepath.Join(artifactDir, store.MirrorSubdir), 0o755); err != nil {
		t.Fatalf("create artifact content: %v", err)
	}

	composeFile := filepath.Join(artifactDir, "docker-compose.yml")
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatalf("write artifact content: %v", err)
	}

	if err := Run(t.Context(), nil, &migrationTestClient{}, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if _, err := os.Stat(composeFile); err != nil {
		t.Fatalf("published artifact content was deleted: %v", err)
	}
}

// TestRun_LeavesOCIArtifactContentUntouched covers the same hazard for an
// OCI source, whose base directory has no mirror at all and is therefore
// always descended into.
func TestRun_LeavesOCIArtifactContentUntouched(t *testing.T) {
	dataDir := t.TempDir()
	artifactDir := filepath.Join(dataDir, "ghcr.io", "owner", "artifact", store.ArtifactsSubdir, "sha256-abc")

	if err := os.MkdirAll(filepath.Join(artifactDir, store.MirrorSubdir), 0o755); err != nil {
		t.Fatalf("create artifact content: %v", err)
	}

	composeFile := filepath.Join(artifactDir, "docker-compose.yml")
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatalf("write artifact content: %v", err)
	}

	if err := Run(t.Context(), nil, &migrationTestClient{}, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if _, err := os.Stat(composeFile); err != nil {
		t.Fatalf("extracted OCI artifact content was deleted: %v", err)
	}
}

// TestRun_MigratesLegacyCheckoutWhoseWorkingTreeBlocksTheMirrorPath covers a
// legacy checkout of a repository that itself tracks a directory called
// "mirror". That entry occupies the exact path the bare mirror has to live
// at, so it must be cleared before the mirror can be created - otherwise not
// just migration but GitStore itself can never create one.
func TestRun_MigratesLegacyCheckoutWhoseWorkingTreeBlocksTheMirrorPath(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initLegacyCheckout(t, repoDir)

	blockingDir := filepath.Join(repoDir, store.MirrorSubdir)
	if _, err := git.PlainInit(blockingDir, false); err != nil {
		t.Fatalf("create nested working-tree repository: %v", err)
	}

	if err := Run(t.Context(), nil, &migrationTestClient{}, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(repoDir, store.MirrorSubdir, "HEAD")); err != nil {
		t.Fatalf("mirror was not bootstrapped over the blocking working-tree directory: %v", err)
	}

	if _, err := os.Stat(filepath.Join(repoDir, gitDirName)); !os.IsNotExist(err) {
		t.Errorf("legacy .git directory should have been moved, stat err = %v", err)
	}
}

func TestRun_MigratesLegacyCheckoutWhoseWorkingTreeFileBlocksMirrorPath(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initLegacyCheckout(t, repoDir)

	if err := os.WriteFile(filepath.Join(repoDir, store.MirrorSubdir), []byte("tracked\n"), 0o600); err != nil {
		t.Fatalf("write blocking working-tree file: %v", err)
	}

	if err := Run(t.Context(), nil, &migrationTestClient{}, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if _, err := git.PlainOpen(filepath.Join(repoDir, store.MirrorSubdir)); err != nil {
		t.Fatalf("open bootstrapped mirror: %v", err)
	}
}

// TestRun_KeepsBlockingWorkingTreeReferencedByRunningContainer ensures the
// blocked path above still defers to a running container: clearing the
// working tree is only safe once nothing is deployed from it.
func TestRun_KeepsBlockingWorkingTreeReferencedByRunningContainer(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initLegacyCheckout(t, repoDir)

	blockingFile := filepath.Join(repoDir, store.MirrorSubdir, "tracked.txt")
	if err := os.MkdirAll(filepath.Dir(blockingFile), 0o755); err != nil {
		t.Fatalf("create blocking working-tree directory: %v", err)
	}

	if err := os.WriteFile(blockingFile, []byte("tracked\n"), 0o600); err != nil {
		t.Fatalf("write blocking working-tree file: %v", err)
	}

	apiClient := &migrationTestClient{containers: []container.Summary{{
		Labels: map[string]string{docker.DocoCDLabels.Deployment.WorkingDir: repoDir},
	}}}

	if err := Run(t.Context(), nil, apiClient, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if _, err := os.Stat(blockingFile); err != nil {
		t.Errorf("working-tree file of a still-deployed stack was deleted: %v", err)
	}

	if _, err := os.Stat(filepath.Join(repoDir, gitDirName)); err != nil {
		t.Errorf("legacy .git directory should be kept until the stack is redeployed: %v", err)
	}
}

func TestLabelsReferenceLegacyPath_AcceptsHostAndContainerMountPaths(t *testing.T) {
	t.Parallel()

	containerRepoDir := "/data/github.com/owner/repo"
	hostRepoDir := "/srv/doco-cd/github.com/owner/repo"
	labels := map[string]string{
		docker.DocoCDLabels.Deployment.WorkingDir: filepath.Join(hostRepoDir, "compose"),
	}

	if !labelsReferenceLegacyPath(labels, []string{containerRepoDir, hostRepoDir}, storeLayoutEntries()) {
		t.Fatal("host-side working directory was not recognized as a legacy repository reference")
	}
}

// TestRun_RemovesLegacyArtifactsDirectoryOnBootstrap covers a legacy
// checkout of a repository tracking a directory called "artifacts". Right
// after bootstrapping the mirror out of ".git", nothing under the repository
// directory can be published store content yet, so that entry is working-tree
// content and must not survive as a fake artifacts directory.
func TestRun_RemovesLegacyArtifactsDirectoryOnBootstrap(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initLegacyCheckout(t, repoDir)

	legacyArtifacts := filepath.Join(repoDir, store.ArtifactsSubdir)
	if err := os.MkdirAll(legacyArtifacts, 0o755); err != nil {
		t.Fatalf("create legacy artifacts directory: %v", err)
	}

	if err := Run(t.Context(), nil, &migrationTestClient{}, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if _, err := os.Stat(legacyArtifacts); !os.IsNotExist(err) {
		t.Errorf("legacy working-tree \"artifacts\" directory survived migration, stat err = %v", err)
	}
}

// TestRun_HonorsCanceledContext ensures a shutdown during startup migration
// stops the pass instead of walking the whole data mount point.
func TestRun_HonorsCanceledContext(t *testing.T) {
	dataDir := t.TempDir()
	initLegacyCheckout(t, filepath.Join(dataDir, "github.com", "owner", "repo"))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := Run(ctx, nil, &migrationTestClient{}, dataDir); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}
