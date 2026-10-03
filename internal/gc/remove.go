package gc

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/source"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// RemovalReason tells why RepositoryRemover.RemoveIfUnused removed or kept a repository directory.
type RemovalReason string

const (
	// RemovalReasonUnused means that no deployment used the repository, so the directory was removed.
	RemovalReasonUnused RemovalReason = "unused"
	// RemovalReasonNotFound means that the directory did not exist.
	RemovalReasonNotFound RemovalReason = "not_found"
	// RemovalReasonInUse means that a deployment on a Docker context still references the repository.
	RemovalReasonInUse RemovalReason = "in_use"
	// RemovalReasonInFlight means that a deployment from the repository is in progress and has not labeled its resources yet.
	RemovalReasonInFlight RemovalReason = "in_flight"
	// RemovalReasonLocked means that another deployment or the garbage collector holds the repository.
	RemovalReasonLocked RemovalReason = "locked"
	// RemovalReasonDiscoveryFailed means that a Docker context could not be inspected.
	// The directory is kept because that context can still use it.
	RemovalReasonDiscoveryFailed RemovalReason = "discovery_failed"
	// RemovalReasonError means that the directory could not be checked or removed.
	RemovalReasonError RemovalReason = "error"
)

// errNoContexts makes the default discovery fail closed when no context registry is set.
var errNoContexts = errors.New("no docker context registry configured")

// RepositoryRemoval is the result of RepositoryRemover.RemoveIfUnused.
type RepositoryRemoval struct {
	Removed bool
	Reason  RemovalReason
	// Usages lists the deployments that still use the repository, if Reason is RemovalReasonInUse.
	Usages []Usage
	// InFlight lists the revisions of deployments in progress, if Reason is RemovalReasonInFlight.
	InFlight []store.Revision
}

// RepositoryRemover removes the directory of a repository (mirror, submodule cache, artifacts and
// live files) after a stack was destroyed with destroy.remove_dir.
//
// Other stacks can deploy from the same repository. Their containers and Swarm services bind-mount
// files from the artifacts in that directory. So the directory is only removed if no deployment on
// any configured Docker context references the repository and no deployment from it is in progress.
// This uses the same live set and locks as the Sweeper.
type RepositoryRemover struct {
	contexts             *docker.ContextRegistry
	dataMountSource      string
	dataMountDestination string

	// liveUsages is overridable in tests.
	liveUsages func(ctx context.Context, contexts *docker.ContextRegistry, log *slog.Logger, dataMountSource, dataMountDestination string) (map[string][]Usage, error)
}

// NewRepositoryRemover creates a RepositoryRemover. dataMountSource is the data mount path on the
// Docker host and dataMountDestination is the same path inside the doco-cd container.
func NewRepositoryRemover(contexts *docker.ContextRegistry, dataMountSource, dataMountDestination string) *RepositoryRemover {
	return &RepositoryRemover{
		contexts:             contexts,
		dataMountSource:      dataMountSource,
		dataMountDestination: dataMountDestination,
		liveUsages:           liveUsagesFailClosed,
	}
}

// liveUsagesFailClosed is LiveUsages, but it returns an error if no context registry is set.
// LiveUsages returns an empty result in that case, which would look like "nobody uses the repository".
func liveUsagesFailClosed(
	ctx context.Context,
	contexts *docker.ContextRegistry,
	log *slog.Logger,
	dataMountSource string,
	dataMountDestination string,
) (map[string][]Usage, error) {
	if contexts == nil {
		return nil, errNoContexts
	}

	return LiveUsages(ctx, contexts, log, dataMountSource, dataMountDestination)
}

// RemoveUnused calls RemoveIfUnused for each repository and logs failures. A nil RepositoryRemover does nothing.
// Call it only after the deployment job that requested the removals has released its
// repository locks. Otherwise the GC lock of the job keeps every directory.
func (r *RepositoryRemover) RemoveUnused(ctx context.Context, log *slog.Logger, repoNames []string) {
	if r == nil {
		return
	}

	for _, repoName := range repoNames {
		if ctx.Err() != nil {
			return
		}

		if _, err := r.RemoveIfUnused(ctx, log, repoName); err != nil {
			log.Error("failed to remove repository directory, keeping it",
				slog.String("repository", repoName), logger.ErrAttr(err))
		}
	}
}

// RemoveIfUnused removes the directory of repoName if no deployment uses the repository.
//
// The directory is kept if any of these is true:
//   - another deployment or the garbage collector holds the GC lock of the repository;
//   - a deployment from the repository is in progress;
//   - a container or Swarm service on any Docker context references a revision of the repository
//     (as deployed revision, pinned revision or config revision);
//   - a Docker context cannot be inspected.
//
// Unused artifacts of a kept repository are left to the Sweeper and its retention rules.
// The sibling lock files of the directory are never removed, because other processes can still use them.
func (r *RepositoryRemover) RemoveIfUnused(ctx context.Context, log *slog.Logger, repoName string) (RepositoryRemoval, error) {
	repoLog := log.With(slog.String("repository", repoName))

	repoDir, err := r.repositoryDir(repoName)
	if err != nil {
		return RepositoryRemoval{Reason: RemovalReasonError}, err
	}

	if _, err = os.Stat(repoDir); errors.Is(err, os.ErrNotExist) {
		repoLog.Debug("repository directory does not exist, nothing to remove")
		return RepositoryRemoval{Reason: RemovalReasonNotFound}, nil
	} else if err != nil {
		return RepositoryRemoval{Reason: RemovalReasonError}, fmt.Errorf("check repository directory: %w", err)
	}

	// Every deployment holds a shared GC lock from Prepare until its resources are labeled.
	// An exclusive lock therefore proves that no deployment of this repository is between
	// "artifact published" and "labels visible", so the label scan below sees all users.
	unlockGC, acquired, err := sourcecache.TryAcquireExclusiveGCPathLock(repoDir)
	if err != nil {
		return RepositoryRemoval{Reason: RemovalReasonError}, fmt.Errorf("acquire repository GC lock: %w", err)
	}

	if !acquired {
		repoLog.Info("keeping repository directory, another deployment or the garbage collector is using the repository")
		return RepositoryRemoval{Reason: RemovalReasonLocked}, nil
	}

	defer unlockGC()

	// Prepare takes the GC lock first and then the shared path lock. The same order here prevents a deadlock.
	unlockRepo := sourcecache.AcquireExclusivePathLock(repoDir)
	defer unlockRepo()

	if inFlight, err := r.inFlightRevisions(repoDir, repoName); err != nil {
		return RepositoryRemoval{Reason: RemovalReasonError}, err
	} else if len(inFlight) > 0 {
		repoLog.Info("keeping repository directory, a deployment from the repository is in progress",
			slog.Any("revisions", inFlight))

		return RepositoryRemoval{Reason: RemovalReasonInFlight, InFlight: inFlight}, nil
	}

	live, err := r.liveUsages(ctx, r.contexts, log, r.dataMountSource, r.dataMountDestination)
	if err != nil {
		return RepositoryRemoval{Reason: RemovalReasonDiscoveryFailed},
			fmt.Errorf("discover deployments that use the repository: %w", err)
	}

	if usages := r.usagesFor(live, repoDir, repoName); len(usages) > 0 {
		deployments := make([]string, 0, len(usages))
		for _, usage := range usages {
			deployments = append(deployments, usage.String())
		}

		repoLog.Info("keeping repository directory, other deployments still use the repository",
			slog.Any("deployments", deployments))

		return RepositoryRemoval{Reason: RemovalReasonInUse, Usages: usages}, nil
	}

	repoLog.Debug("removing unused repository directory", slog.String("path", repoDir))

	if err = os.RemoveAll(repoDir); err != nil {
		return RepositoryRemoval{Reason: RemovalReasonError}, fmt.Errorf("remove repository directory: %w", err)
	}

	r.removeEmptyParents(repoDir)

	repoLog.Info("removed unused repository directory")

	return RepositoryRemoval{Removed: true, Reason: RemovalReasonUnused}, nil
}

// repositoryDir returns the directory of repoName below the data mount point.
// It rejects names that resolve to the data mount point itself or to a path outside of it.
func (r *RepositoryRemover) repositoryDir(repoName string) (string, error) {
	if r.dataMountDestination == "" {
		return "", errors.New("data mount point is not set")
	}

	repoDir, err := filesystem.VerifyAndSanitizePath(
		filepath.Join(r.dataMountDestination, repoName),
		r.dataMountDestination,
	)
	if err != nil {
		return "", fmt.Errorf("resolve repository directory: %w", err)
	}

	root, err := filepath.Abs(r.dataMountDestination)
	if err != nil {
		return "", fmt.Errorf("resolve data mount point: %w", err)
	}

	if repoDir == root {
		return "", fmt.Errorf("resolve repository directory: invalid repository name %q", repoName)
	}

	return repoDir, nil
}

// relativeName returns the path of repoDir relative to the data mount point.
// The sweeper and the label discovery use this form of the repository name.
func (r *RepositoryRemover) relativeName(repoDir string) string {
	rel, err := filepath.Rel(r.dataMountDestination, repoDir)
	if err != nil {
		return ""
	}

	return filepath.ToSlash(rel)
}

// inFlightRevisions returns the revisions under repoDir that a deployment marked in flight.
// Prepare marks revisions under the unsanitized repository name, so both names are checked.
func (r *RepositoryRemover) inFlightRevisions(repoDir, repoName string) ([]store.Revision, error) {
	artifacts, err := store.ListArtifacts(repoDir)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}

	relName := r.relativeName(repoDir)

	var inFlight []store.Revision

	for _, artifact := range artifacts {
		revision := string(artifact.Revision)
		if source.IsInFlight(repoName, revision) || source.IsInFlight(relName, revision) {
			inFlight = append(inFlight, artifact.Revision)
		}
	}

	return inFlight, nil
}

// usagesFor returns the usages of the repository stored at repoDir, sorted for stable log output.
// It uses the same key matching as liveRevisionsFor, so the remover and the sweeper agree on
// which deployments use a repository.
func (r *RepositoryRemover) usagesFor(live map[string][]Usage, repoDir, repoName string) []Usage {
	relName := r.relativeName(repoDir)

	var usages []Usage

	for key, keyUsages := range live {
		if repositoryKeyMatches(relName, key) || repositoryKeyMatches(repoName, key) {
			usages = append(usages, keyUsages...)
		}
	}

	slices.SortFunc(usages, func(a, b Usage) int {
		return cmp.Or(
			cmp.Compare(a.Context, b.Context),
			cmp.Compare(a.Stack, b.Stack),
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Revision, b.Revision),
		)
	})

	return slices.Compact(usages)
}

// removeEmptyParents removes the parent directories of repoDir that are empty now.
// Repository names are hierarchical ("<host>/<owner>/<repo>"), so the removed repository can be
// the last entry of its parents. A directory that is not empty (another repository or the lock
// files of this repository, which must stay) stops the walk.
func (r *RepositoryRemover) removeEmptyParents(repoDir string) {
	root := filepath.Clean(r.dataMountDestination)

	for dir := filepath.Dir(repoDir); dir != root && filesystem.InBasePath(root, dir); dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			return
		}
	}
}
