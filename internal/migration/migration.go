// Package migration converts legacy repository checkouts to the current mirror-and-artifact layout during startup. It
// reuses the existing Git objects and removes old working-tree files only when they are no longer used.
package migration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/logger"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// gitDirName is the on-disk directory name of a non-bare repository's git
// directory - the exact layout every old doco-cd checkout used
// directly under "<DataMountPoint>/<repoName>".
const gitDirName = ".git"

// Run migrates legacy repositories below dataMountDestination in place. It is safe to call on every startup and leaves
// current stores and unrelated directories untouched. Per-repository failures are logged and do not stop the migration
// pass or application startup.
func Run(ctx context.Context, log *slog.Logger, apiClient client.APIClient, dataMountDestination string) error {
	return run(ctx, log, dataMountDestination, func(ctx context.Context, repoDir string, keep []string) (bool, error) {
		return referencedByRunningContainer(ctx, apiClient, repoDir, keep)
	})
}

// RunWithContexts migrates legacy repositories while checking every configured Docker context before removing old
// working-tree files. Both mount paths are checked because deployment labels contain daemon-visible host paths.
// Discovery failures leave the files untouched.
func RunWithContexts(
	ctx context.Context,
	log *slog.Logger,
	contexts *docker.ContextRegistry,
	dataMountSource string,
	dataMountDestination string,
) error {
	return run(ctx, log, dataMountDestination, func(ctx context.Context, repoDir string, keep []string) (bool, error) {
		relativeRepoDir, err := filepath.Rel(dataMountDestination, repoDir)
		if err != nil {
			return false, fmt.Errorf("derive host repository path: %w", err)
		}

		hostRepoDir := filepath.Join(dataMountSource, relativeRepoDir)

		return referencedAcrossContexts(ctx, contexts, []string{repoDir, hostRepoDir}, keep)
	})
}

// LeftoverTracker remembers, for the lifetime of the process, which repository directories are
// already known to be free of legacy leftovers, so CleanupRepoLeftovers can skip redundant
// checks for them. A repository can never regain a legacy on-disk layout once migrated, so a
// "clean" entry stays valid forever; the map only grows bounded by the number of distinct
// repository directories ever deployed.
type LeftoverTracker struct {
	mu    sync.Mutex
	repos map[string]*leftoverState
}

type leftoverState struct {
	mu    sync.Mutex
	clean bool
}

// NewLeftoverTracker creates an empty LeftoverTracker.
func NewLeftoverTracker() *LeftoverTracker {
	return &LeftoverTracker{repos: make(map[string]*leftoverState)}
}

// lock serializes cleanup attempts for the same repository while still allowing different
// repositories to be cleaned concurrently. The caller must invoke the returned unlock function.
func (t *LeftoverTracker) lock(repoDir string) (*leftoverState, func()) {
	if t == nil {
		return nil, func() {}
	}

	t.mu.Lock()

	state := t.repos[repoDir]
	if state == nil {
		state = &leftoverState{}
		t.repos[repoDir] = state
	}

	t.mu.Unlock()

	state.mu.Lock()

	return state, state.mu.Unlock
}

// isClean reports whether repoDir is already known to be free of legacy leftovers.
func (t *LeftoverTracker) isClean(repoDir string) bool {
	if t == nil {
		return false
	}

	t.mu.Lock()

	state := t.repos[repoDir]

	t.mu.Unlock()

	if state == nil {
		return false
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	return state.clean
}

// markClean records that repoDir is already known to be free of legacy leftovers.
func (t *LeftoverTracker) markClean(repoDir string) {
	state, unlock := t.lock(repoDir)
	defer unlock()

	if state != nil {
		state.clean = true
	}
}

// CleanupRepoLeftovers removes legacy on-disk leftovers for a single already-migrated
// repository directory (repoDir), unless they are still bind-mounted by a running container in
// any configured Docker context. It is safe to call after every deployment/destroy of repoDir:
// repositories that were never on the legacy layout, or that have no leftovers, return after at
// most a single directory read - and, once tracker has recorded repoDir as clean, without any
// filesystem or Docker access at all.
//
// Unlike Run/RunWithContexts, it does not walk the whole data mount point and does not acquire
// its own GC lock - callers must already hold at least a shared GC lock on repoDir for the
// duration of the call (e.g. the deployment pipeline's own stage-1 lock), which already excludes
// a concurrent GC sweep from touching the same directory.
func CleanupRepoLeftovers(
	ctx context.Context,
	log *slog.Logger,
	tracker *LeftoverTracker,
	contexts *docker.ContextRegistry,
	dataMountSource string,
	dataMountDestination string,
	repoDir string,
) error {
	return cleanupRepoLeftovers(ctx, log, tracker, repoDir,
		func(ctx context.Context, repoDir string, keep []string) (bool, error) {
			relativeRepoDir, err := filepath.Rel(dataMountDestination, repoDir)
			if err != nil {
				return false, fmt.Errorf("derive host repository path: %w", err)
			}

			hostRepoDir := filepath.Join(dataMountSource, relativeRepoDir)

			return referencedAcrossContexts(ctx, contexts, []string{repoDir, hostRepoDir}, keep)
		})
}

// cleanupRepoLeftovers is the source-agnostic implementation behind CleanupRepoLeftovers,
// parameterized over the reference check so it can be unit-tested with a fake isReferenced,
// matching the pattern run uses for Run/RunWithContexts.
func cleanupRepoLeftovers(ctx context.Context, log *slog.Logger, tracker *LeftoverTracker, repoDir string, isReferenced referenceChecker) error {
	state, unlock := tracker.lock(repoDir)
	defer unlock()

	if state != nil && state.clean {
		return nil
	}

	if log == nil {
		log = slog.Default()
	}

	migrated, err := isStoreRoot(repoDir)
	if err != nil {
		return fmt.Errorf("inspect mirror directory: %w", err)
	}

	if !migrated {
		// Not (yet) on the migrated store layout - nothing for this function to clean up. Do not
		// mark it clean: it may simply not have finished migrating yet.
		return nil
	}

	clean, err := cleanupLegacyLeftovers(ctx, log, isReferenced, repoDir, storeLayoutEntries())
	if err != nil {
		return err
	}

	if clean {
		if state != nil {
			state.clean = true
		}
	}

	return nil
}

type referenceChecker func(ctx context.Context, repoDir string, keep []string) (bool, error)

func run(ctx context.Context, log *slog.Logger, dataMountDestination string, isReferenced referenceChecker) error {
	if log == nil {
		log = slog.Default()
	}

	if _, err := os.Stat(dataMountDestination); err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("migration: stat data mount point %s: %w", dataMountDestination, err)
	}

	return filepath.WalkDir(dataMountDestination, func(path string, d os.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		if err != nil {
			log.Warn("failed to walk data mount point; skipping subtree", slog.String("path", path), logger.ErrAttr(err))
			return nil
		}

		if path == dataMountDestination || !d.IsDir() {
			return nil
		}

		isRepoRoot, checkErr := looksLikeRepoRoot(path)
		if checkErr != nil {
			log.Warn("failed to check directory for a legacy repository layout; skipping",
				slog.String("path", path), logger.ErrAttr(checkErr))

			return nil
		}

		if !isRepoRoot {
			return nil
		}

		repoLog := log.With(slog.String("repo_dir", path))

		unlockGC, lockErr := sourcecache.AcquireExclusiveGCPathLock(path)
		if lockErr != nil {
			repoLog.Warn("failed to acquire repository GC lock; leaving it untouched for now",
				logger.ErrAttr(lockErr))

			return filepath.SkipDir
		}

		migrateErr := migrateRepo(ctx, repoLog, isReferenced, path)

		unlockGC()

		if migrateErr != nil {
			repoLog.Warn("failed to migrate legacy repository layout; leaving it untouched for now",
				logger.ErrAttr(migrateErr))
		}

		return filepath.SkipDir
	})
}

// looksLikeRepoRoot reports whether path itself is a doco-cd Git source's on-disk root,
// at whatever depth under dataMountDestination git.GetRepoName happened to place it:
// either a legacy checkout (a ".git" directory) or an already-migrated store
// (a "mirror" directory that really is a git directory).
//
// The "mirror" marker is deliberately verified rather than just stat'd. The
// walk descends into published artifacts as well, and a deployed repository
// is free to contain an ordinary directory of its own called "mirror";
// treating that as a store root would make migrateRepo mistake a published
// artifact's contents for a legacy working tree and delete them.
func looksLikeRepoRoot(path string) (bool, error) {
	legacy, err := isDir(filepath.Join(path, gitDirName))
	if err != nil || legacy {
		return legacy, err
	}

	return isStoreRoot(path)
}

// isDir reports whether path exists and is a directory.
func isDir(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}

		return false, err
	}

	return info.IsDir(), nil
}

// isGitDir reports whether path is a valid Git directory, bare or not.
func isGitDir(path string) (bool, error) {
	dir, err := isDir(path)
	if err != nil || !dir {
		return false, err
	}

	if _, err := git.PlainOpen(path); err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

// isStoreRoot reports whether path is a doco-cd Git source's on-disk root in the new store layout:
// a "mirror" directory that really is a git directory, and a mirror lock file.
func isStoreRoot(path string) (bool, error) {
	mirror, err := isGitDir(filepath.Join(path, store.MirrorSubdir))
	if err != nil || !mirror {
		return mirror, err
	}

	return pathExists(filepath.Join(path, store.MirrorSubdir+".lock"))
}

// storeLayoutEntries are the entries of a repository directory that belong
// to the new store layout rather than to a legacy checkout's working tree:
// the bare mirror, published artifacts, submodule cache, and mirror lock.
func storeLayoutEntries() []string {
	return []string{
		store.MirrorSubdir,
		store.ArtifactsSubdir,
		store.SubmodulesSubdir,
		store.MirrorSubdir + ".lock",
	}
}

// migrateRepo migrates a single repository directory: it bootstraps a bare
// mirror from a legacy checkout's ".git" directory if one hasn't been
// created yet, then cleans up any leftover legacy working-tree files that
// are no longer bind-mounted by a running container.
func migrateRepo(ctx context.Context, log *slog.Logger, isReferenced referenceChecker, repoDir string) error {
	mirrorDir := filepath.Join(repoDir, store.MirrorSubdir)
	legacyGitDir := filepath.Join(repoDir, gitDirName)

	legacy, err := isGitDir(legacyGitDir)
	if err != nil {
		return fmt.Errorf("inspect legacy git directory: %w", err)
	}

	if legacy {
		// A legacy checkout takes precedence over anything in its working tree named "mirror", including nested Git
		// repositories. Every entry other than ".git" is legacy working-tree content.
		blocked, err := pathExists(mirrorDir)
		if err != nil {
			return fmt.Errorf("inspect mirror path: %w", err)
		}

		if blocked {
			removed, err := cleanupLegacyLeftovers(ctx, log, isReferenced, repoDir, []string{gitDirName})
			if err != nil {
				return err
			}

			if !removed {
				return nil
			}

			return bootstrapMirror(log, legacyGitDir, mirrorDir)
		}

		if err := bootstrapMirror(log, legacyGitDir, mirrorDir); err != nil {
			return fmt.Errorf("bootstrap mirror: %w", err)
		}

		_, err = cleanupLegacyLeftovers(ctx, log, isReferenced, repoDir,
			[]string{store.MirrorSubdir, store.MirrorSubdir + ".lock"})

		return err
	}

	migrated, err := isStoreRoot(repoDir)
	if err != nil {
		return fmt.Errorf("inspect mirror directory: %w", err)
	}

	if migrated {
		_, err := cleanupLegacyLeftovers(ctx, log, isReferenced, repoDir, storeLayoutEntries())

		return err
	}

	// Neither a migrated store nor a legacy checkout.
	return nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}

	if os.IsNotExist(err) {
		return false, nil
	}

	return false, err
}

// bootstrapMirror converts a legacy non-bare checkout into the new bare
// mirror layout by renaming its ".git" directory into place as mirrorDir.
// This never touches the network: the mirror's objects and refs are exactly
// what the legacy checkout already had on disk, and GitStore's normal fetch
// path incrementally brings it up to date the next time it is used.
func bootstrapMirror(log *slog.Logger, legacyGitDir, mirrorDir string) error {
	// Hold the same lock GitStore's own mirror operations use, so this can
	// never race a concurrent CloneOrUpdateBareMirror for the same path.
	// Migration runs before doco-cd starts accepting webhooks or polling, so
	// this is normally uncontended; it only matters for a manual/parallel
	// invocation.
	unlock := sourcecache.AcquirePathLock(mirrorDir)
	defer unlock()

	// Re-check under the lock: a concurrent caller may have already migrated it.
	if migrated, err := isGitDir(mirrorDir); err != nil {
		return fmt.Errorf("inspect mirror directory: %w", err)
	} else if migrated {
		return nil
	}

	log.Info("migrating legacy checkout to bare mirror layout", slog.String("mirror_dir", mirrorDir))

	if err := os.Rename(legacyGitDir, mirrorDir); err != nil {
		return fmt.Errorf("rename %s to %s: %w", legacyGitDir, mirrorDir, err)
	}

	if err := markBare(mirrorDir); err != nil {
		// The rename already succeeded and nothing in this codebase depends
		// on this config flag - bareness is structural (no nested ".git" inside mirrorDir)
		// as far as go-git and GitStore are concerned - so a failure here is cosmetic only.
		log.Warn("failed to mark migrated mirror as bare in its git config", logger.ErrAttr(err))
	}

	return nil
}

// markBare updates the migrated mirror's git config to reflect that it is
// now a bare repository with no associated worktree, matching what a fresh
// GitStore-created mirror looks like.
func markBare(mirrorDir string) error {
	repo, err := git.PlainOpen(mirrorDir)
	if err != nil {
		return fmt.Errorf("open migrated mirror: %w", err)
	}

	cfg, err := repo.Config()
	if err != nil {
		return fmt.Errorf("read migrated mirror config: %w", err)
	}

	cfg.Core.IsBare = true
	cfg.Core.Worktree = ""

	if err := repo.SetConfig(cfg); err != nil {
		return fmt.Errorf("write migrated mirror config: %w", err)
	}

	return nil
}

// legacyLeftoverEntries returns every immediate entry of repoDir that is not in keep -
// exactly the working-tree files a migrated legacy checkout leaves behind.
func legacyLeftoverEntries(repoDir string, keep []string) ([]string, error) {
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return nil, err
	}

	var leftovers []string

	for _, e := range entries {
		if slices.Contains(keep, e.Name()) {
			continue
		}

		leftovers = append(leftovers, e.Name())
	}

	return leftovers, nil
}

// cleanupLegacyLeftovers removes every entry of repoDir that is not in keep,
// but only once no currently running container's working-directory label
// still points at one of them. It reports whether repoDir is now free of
// such entries, so a caller that needs them gone before it can continue
// (see migrateRepo's handling of a working-tree entry occupying the
// mirror's own path) can tell a successful cleanup from a deferred one.
func cleanupLegacyLeftovers(ctx context.Context, log *slog.Logger, isReferenced referenceChecker, repoDir string, keep []string) (bool, error) {
	leftovers, err := legacyLeftoverEntries(repoDir, keep)
	if err != nil {
		return false, fmt.Errorf("list legacy leftovers: %w", err)
	}

	if len(leftovers) == 0 {
		return true, nil
	}

	referenced, err := isReferenced(ctx, repoDir, keep)
	if err != nil {
		return false, fmt.Errorf("check running containers for legacy references: %w", err)
	}

	if referenced {
		log.Debug("legacy checkout files are still bind-mounted by a running container; leaving them until it is redeployed")
		return false, nil
	}

	log.Info("removing legacy checkout files left over after migration", slog.Any("entries", leftovers))

	var errs []error

	for _, name := range leftovers {
		if err := os.RemoveAll(filepath.Join(repoDir, name)); err != nil {
			errs = append(errs, fmt.Errorf("remove legacy entry %s: %w", name, err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return false, err
	}

	return true, nil
}

// referencedByRunningContainer reports whether any container currently
// known to the default Docker context (running or not - a stopped container
// can still be started again) has a working-directory label pointing inside
// repoDir, outside the entries in keep.
//
// It only inspects the default context: doco-cd cannot know which other
// contexts a repository's stacks might be deployed to until their deploy
// configs are read from the very store this migration is bootstrapping. A
// remote-context stack's legacy files are therefore left in place by this
// pass; they are picked up by a later restart once the evidence used here
// (the default context) no longer conflicts, or once that stack has been
// redeployed and stopped referencing them by any other means.
func referencedByRunningContainer(ctx context.Context, apiClient client.APIClient, repoDir string, keep []string) (bool, error) {
	result, err := apiClient.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return false, err
	}

	for _, cont := range result.Items {
		if labelsReferenceLegacyPath(cont.Labels, []string{repoDir}, keep) {
			return true, nil
		}
	}

	return false, nil
}

func referencedAcrossContexts(ctx context.Context, contexts *docker.ContextRegistry, repoDirs []string, keep []string) (bool, error) {
	if contexts == nil {
		return false, errors.New("docker context registry is unavailable")
	}

	results, err := contexts.List(ctx)
	if err != nil {
		return false, fmt.Errorf("list docker contexts: %w", err)
	}

	for _, result := range results {
		if result.Err != nil {
			return false, fmt.Errorf("inspect docker context %s: %w", result.DisplayName(), result.Err)
		}

		if result.Cli == nil {
			return false, fmt.Errorf("inspect docker context %s: missing client", result.DisplayName())
		}

		modes := []bool{false}
		if result.SwarmMode {
			modes = append(modes, true)
		}

		for _, swarmMode := range modes {
			referenced, err := referencedOnContext(ctx, result.Cli.Client(), swarmMode, repoDirs, keep)
			if err != nil {
				return false, fmt.Errorf("inspect deployments in docker context %s: %w", result.DisplayName(), err)
			}

			if referenced {
				return true, nil
			}
		}
	}

	return false, nil
}

func referencedOnContext(
	ctx context.Context,
	apiClient client.APIClient,
	swarmMode bool,
	repoDirs []string,
	keep []string,
) (bool, error) {
	services, err := docker.GetServicesWithLabelKey(
		ctx,
		apiClient,
		swarmMode,
		docker.DocoCDLabels.Deployment.WorkingDir,
	)
	if err != nil {
		return false, fmt.Errorf("list deployments: %w", err)
	}

	for _, labels := range services {
		if labelsReferenceLegacyPath(labels, repoDirs, keep) {
			return true, nil
		}
	}

	if !swarmMode {
		return false, nil
	}

	// A Swarm service update changes the current service spec before every old task has
	// necessarily stopped. Those old tasks retain the previous ContainerSpec labels and may
	// still have a legacy directory bind-mounted, so service labels alone are not sufficient.
	tasks, err := apiClient.TaskList(ctx, client.TaskListOptions{})
	if err != nil {
		return false, fmt.Errorf("list swarm tasks: %w", err)
	}

	for _, task := range tasks.Items {
		if !isActiveSwarmTask(task) || task.Spec.ContainerSpec == nil {
			continue
		}

		if labelsReferenceLegacyPath(task.Spec.ContainerSpec.Labels, repoDirs, keep) {
			return true, nil
		}
	}

	return false, nil
}

func isActiveSwarmTask(task swarmTypes.Task) bool {
	switch task.Status.State {
	case swarmTypes.TaskStateComplete, swarmTypes.TaskStateShutdown, swarmTypes.TaskStateFailed,
		swarmTypes.TaskStateRejected, swarmTypes.TaskStateOrphaned, swarmTypes.TaskStateRemove:
		return false
	default:
		return true
	}
}

func labelsReferenceLegacyPath(labels map[string]string, repoDirs []string, keep []string) bool {
	workingDir := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.WorkingDir])
	if workingDir == "" {
		return false
	}

	for _, repoDir := range repoDirs {
		if !filesystem.InBasePath(repoDir, workingDir) {
			continue
		}

		return !slices.ContainsFunc(keep, func(name string) bool {
			return filesystem.InBasePath(filepath.Join(repoDir, name), workingDir)
		})
	}

	return false
}
