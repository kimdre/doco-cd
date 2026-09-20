package migration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/moby/moby/api/types/container"
	swarmTypes "github.com/moby/moby/api/types/swarm"
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
	services   []swarmTypes.Service
	tasks      []swarmTypes.Task
}

func (c *migrationTestClient) ContainerList(_ context.Context, _ client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: c.containers}, nil
}

func (c *migrationTestClient) ServiceList(_ context.Context, _ client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{Items: c.services}, nil
}

func (c *migrationTestClient) TaskList(_ context.Context, _ client.TaskListOptions) (client.TaskListResult, error) {
	return client.TaskListResult{Items: c.tasks}, nil
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

func TestReferencedOnContext_SwarmProtectsLegacyPathUsedByActiveOldTask(t *testing.T) {
	t.Parallel()

	repoDir := "/data/github.com/owner/repo"
	artifactDir := filepath.Join(repoDir, store.ArtifactsSubdir, "new-revision")

	apiClient := &migrationTestClient{
		services: []swarmTypes.Service{{
			Spec: swarmTypes.ServiceSpec{
				Annotations: swarmTypes.Annotations{
					Name: "app",
					Labels: map[string]string{
						docker.DocoCDLabels.Deployment.WorkingDir: artifactDir,
					},
				},
			},
		}},
		tasks: []swarmTypes.Task{{
			Status: swarmTypes.TaskStatus{State: swarmTypes.TaskStateRunning},
			Spec: swarmTypes.TaskSpec{
				ContainerSpec: &swarmTypes.ContainerSpec{
					Labels: map[string]string{
						docker.DocoCDLabels.Deployment.WorkingDir: repoDir,
					},
				},
			},
		}},
	}

	referenced, err := referencedOnContext(t.Context(), apiClient, true, []string{repoDir}, storeLayoutEntries())
	if err != nil {
		t.Fatalf("referencedOnContext() error = %v", err)
	}

	if !referenced {
		t.Fatal("active old-spec Swarm task using the legacy path was not detected")
	}
}

func TestReferencedOnContext_SwarmIgnoresTerminalOldTask(t *testing.T) {
	t.Parallel()

	repoDir := "/data/github.com/owner/repo"

	apiClient := &migrationTestClient{
		tasks: []swarmTypes.Task{{
			Status: swarmTypes.TaskStatus{State: swarmTypes.TaskStateShutdown},
			Spec: swarmTypes.TaskSpec{
				ContainerSpec: &swarmTypes.ContainerSpec{
					Labels: map[string]string{
						docker.DocoCDLabels.Deployment.WorkingDir: repoDir,
					},
				},
			},
		}},
	}

	referenced, err := referencedOnContext(t.Context(), apiClient, true, []string{repoDir}, storeLayoutEntries())
	if err != nil {
		t.Fatalf("referencedOnContext() error = %v", err)
	}

	if referenced {
		t.Fatal("terminal old-spec Swarm task must not keep legacy files alive")
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

// TestRun_RemovesNonEmptyLegacyArtifactsDirectoryBeforeBootstrap reproduces a
// legacy checkout whose own working tree already has a non-empty "artifacts"
// directory (e.g. a repo that happens to publish its own build output under
// that name). Its content must be cleared before the mirror is bootstrapped;
// otherwise it would go on to silently merge into the store's own artifacts
// directory of the same name once doco-cd starts writing to it, and would
// never be recognized as legacy leftover content again.
func TestRun_RemovesNonEmptyLegacyArtifactsDirectoryBeforeBootstrap(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initLegacyCheckout(t, repoDir)

	legacyArtifacts := filepath.Join(repoDir, store.ArtifactsSubdir)
	if err := os.MkdirAll(legacyArtifacts, 0o755); err != nil {
		t.Fatalf("create legacy artifacts directory: %v", err)
	}

	dummyFile := filepath.Join(legacyArtifacts, "dummy.txt")
	if err := os.WriteFile(dummyFile, []byte("dummy\n"), 0o600); err != nil {
		t.Fatalf("write legacy artifacts content: %v", err)
	}

	if err := Run(t.Context(), nil, &migrationTestClient{}, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if _, err := git.PlainOpen(filepath.Join(repoDir, store.MirrorSubdir)); err != nil {
		t.Fatalf("open bootstrapped mirror: %v", err)
	}

	if _, err := os.Stat(dummyFile); !os.IsNotExist(err) {
		t.Errorf("legacy content inside working-tree \"artifacts\" directory survived migration, stat err = %v", err)
	}
}

// TestRun_KeepsLegacyArtifactsDirectoryReferencedByRunningContainer ensures a
// non-empty working-tree "artifacts" directory that collides with the store
// layout still defers to a running container the same way a colliding
// "mirror" directory does: migration must not bootstrap the mirror (or
// delete anything) until the stack is no longer deployed from repoDir.
func TestRun_KeepsLegacyArtifactsDirectoryReferencedByRunningContainer(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initLegacyCheckout(t, repoDir)

	legacyArtifacts := filepath.Join(repoDir, store.ArtifactsSubdir)
	if err := os.MkdirAll(legacyArtifacts, 0o755); err != nil {
		t.Fatalf("create legacy artifacts directory: %v", err)
	}

	dummyFile := filepath.Join(legacyArtifacts, "dummy.txt")
	if err := os.WriteFile(dummyFile, []byte("dummy\n"), 0o600); err != nil {
		t.Fatalf("write legacy artifacts content: %v", err)
	}

	apiClient := &migrationTestClient{containers: []container.Summary{{
		Labels: map[string]string{docker.DocoCDLabels.Deployment.WorkingDir: repoDir},
	}}}

	if err := Run(t.Context(), nil, apiClient, dataDir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if _, err := os.Stat(dummyFile); err != nil {
		t.Errorf("working-tree content of a still-deployed stack was deleted: %v", err)
	}

	if _, err := os.Stat(filepath.Join(repoDir, gitDirName)); err != nil {
		t.Errorf("legacy .git directory should be kept until the stack is redeployed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(repoDir, store.MirrorSubdir, "HEAD")); !os.IsNotExist(err) {
		t.Errorf("mirror should not be bootstrapped while blocking content is still referenced, stat err = %v", err)
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

// TestCleanupRepoLeftovers_NotMigratedIsANoOp covers a repository directory that never went
// through migration (no mirror at all): cleanupRepoLeftovers must not touch it or call
// isReferenced.
func TestCleanupRepoLeftovers_NotMigratedIsANoOp(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("create repo dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repoDir, "some-file"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	called := false
	isReferenced := func(context.Context, string, []string) (bool, error) {
		called = true
		return false, nil
	}

	tracker := NewLeftoverTracker()

	if err := cleanupRepoLeftovers(t.Context(), nil, tracker, repoDir, isReferenced); err != nil {
		t.Fatalf("cleanupRepoLeftovers() error = %v", err)
	}

	if called {
		t.Error("isReferenced must not be called for a repository directory that isn't a migrated store")
	}

	if tracker.isClean(repoDir) {
		t.Error("a not-yet-migrated repository directory must not be marked clean")
	}
}

// TestCleanupRepoLeftovers_NoLeftoversDoesNotCheckReferences covers the steady-state case: a
// migrated store with no legacy leftovers must not call isReferenced at all (the cheap
// legacyLeftoverEntries check is enough), and must be marked clean.
func TestCleanupRepoLeftovers_NoLeftoversDoesNotCheckReferences(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initMigratedStore(t, repoDir)

	called := false
	isReferenced := func(context.Context, string, []string) (bool, error) {
		called = true
		return false, nil
	}

	tracker := NewLeftoverTracker()

	if err := cleanupRepoLeftovers(t.Context(), nil, tracker, repoDir, isReferenced); err != nil {
		t.Fatalf("cleanupRepoLeftovers() error = %v", err)
	}

	if called {
		t.Error("isReferenced must not be called when there are no legacy leftovers")
	}

	if !tracker.isClean(repoDir) {
		t.Error("expected repoDir to be marked clean")
	}
}

// TestCleanupRepoLeftovers_RemovesUnreferencedLeftoversAndMarksClean covers a migrated store
// with a legacy leftover that is not referenced by any running container: it must be removed and
// the tracker must record repoDir as clean afterward.
func TestCleanupRepoLeftovers_RemovesUnreferencedLeftoversAndMarksClean(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initMigratedStore(t, repoDir)

	leftover := filepath.Join(repoDir, "docker-compose.yml")
	if err := os.WriteFile(leftover, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatalf("write legacy leftover: %v", err)
	}

	isReferenced := func(context.Context, string, []string) (bool, error) {
		return false, nil
	}

	tracker := NewLeftoverTracker()

	if err := cleanupRepoLeftovers(t.Context(), nil, tracker, repoDir, isReferenced); err != nil {
		t.Fatalf("cleanupRepoLeftovers() error = %v", err)
	}

	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("expected unreferenced legacy leftover to be removed, stat err = %v", err)
	}

	if !tracker.isClean(repoDir) {
		t.Error("expected repoDir to be marked clean after removing its last leftover")
	}
}

// TestCleanupRepoLeftovers_KeepsReferencedLeftoversAndDoesNotMarkClean covers a migrated store
// whose legacy leftover is still referenced by a running container: the leftover must survive
// and the tracker must not mark repoDir clean, so it is rechecked on the next call.
func TestCleanupRepoLeftovers_KeepsReferencedLeftoversAndDoesNotMarkClean(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	initMigratedStore(t, repoDir)

	leftover := filepath.Join(repoDir, "docker-compose.yml")
	if err := os.WriteFile(leftover, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatalf("write legacy leftover: %v", err)
	}

	callCount := 0
	isReferenced := func(context.Context, string, []string) (bool, error) {
		callCount++
		return true, nil
	}

	tracker := NewLeftoverTracker()

	for i := range 2 {
		if err := cleanupRepoLeftovers(t.Context(), nil, tracker, repoDir, isReferenced); err != nil {
			t.Fatalf("cleanupRepoLeftovers() call %d error = %v", i, err)
		}
	}

	if _, err := os.Stat(leftover); err != nil {
		t.Fatalf("expected referenced legacy leftover to survive: %v", err)
	}

	if tracker.isClean(repoDir) {
		t.Error("a repository still blocked by a running container must not be marked clean")
	}

	if callCount != 2 {
		t.Errorf("expected isReferenced to be called on every retry while blocked, got %d calls", callCount)
	}
}

// TestCleanupRepoLeftovers_SkipsAlreadyCleanRepoWithoutAnyIO covers the tracker short-circuit:
// once a repoDir is marked clean, subsequent calls must not touch the filesystem or call
// isReferenced at all, even if the directory changes on disk afterward.
func TestCleanupRepoLeftovers_SkipsAlreadyCleanRepoWithoutAnyIO(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")

	tracker := NewLeftoverTracker()
	tracker.markClean(repoDir)

	// repoDir does not even exist on disk; a real check would fail trying to inspect it.
	called := false
	isReferenced := func(context.Context, string, []string) (bool, error) {
		called = true
		return false, nil
	}

	if err := cleanupRepoLeftovers(t.Context(), nil, tracker, repoDir, isReferenced); err != nil {
		t.Fatalf("cleanupRepoLeftovers() error = %v", err)
	}

	if called {
		t.Error("isReferenced must not be called for a repoDir already marked clean")
	}
}
