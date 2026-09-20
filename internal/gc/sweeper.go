package gc

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/source"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// Sweeper periodically removes unreferenced, expired published source
// artifacts under every repository/artifact directory it finds beneath the
// data mount point. See the package doc for the retention/liveness model.
//
// Repositories are swept sequentially, same as internal/certrotation.Watcher
// processes contexts sequentially: a sweep is expected to comfortably fit
// within one interval, and sequential sweeping keeps the implementation
// simple to reason about without needing extra coordination.
type Sweeper struct {
	contexts        *docker.ContextRegistry
	log             *slog.Logger
	dataMountSource string
	dataMountPoint  string
	opts            store.GCOptions
	interval        time.Duration

	// now, listRepos, liveRevisions and sweepDirectory are overridable in tests.
	now            func() time.Time
	listRepos      func(dataMountPoint string) ([]string, error)
	liveRevisions  func(ctx context.Context, contexts *docker.ContextRegistry, log *slog.Logger, dataMountSource, dataMountDestination string) (map[string]set.Set[store.Revision], error)
	sweepDirectory func(baseDir string, live set.Set[store.Revision], opts store.GCOptions, now time.Time) (store.GCResult, error)
}

// New creates a Sweeper. dataMountPoint is the (in-container) destination
// path holding one subdirectory per repository/artifact, each in turn
// holding "artifacts/<revision>" (see internal/source/store). interval is
// how often the sweeper runs after its initial startup pass.
func New(
	contexts *docker.ContextRegistry,
	log *slog.Logger,
	dataMountSource string,
	dataMountPoint string,
	opts store.GCOptions,
	interval time.Duration,
) *Sweeper {
	return &Sweeper{
		contexts:        contexts,
		log:             log.With(slog.String("component", "gc")),
		dataMountSource: dataMountSource,
		dataMountPoint:  dataMountPoint,
		opts:            opts,
		interval:        interval,
		now:             time.Now,
		listRepos:       listRepositoryDirs,
		liveRevisions:   LiveRevisions,
		sweepDirectory:  store.Sweep,
	}
}

// Start runs the sweeper loop until ctx is canceled. It performs an initial
// sweep immediately, then resweeps every interval. Callers are expected to
// run Start in its own goroutine, e.g. via graceful.SafeGo, which handles
// WaitGroup bookkeeping.
func (s *Sweeper) Start(ctx context.Context) {
	if s.contexts == nil || s.log == nil || s.dataMountPoint == "" {
		return
	}

	s.log.Info("starting artifact garbage collector",
		slog.Int("retention_records", s.opts.RetentionRecords),
		slog.String("retention_ttl", s.opts.RetentionTTL.String()),
		slog.String("interval", s.interval.String()),
	)

	s.sweep(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("artifact garbage collector stopped")
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// sweep scans and sweeps each repository while holding its exclusive GC gate.
func (s *Sweeper) sweep(ctx context.Context) {
	repoDirs, err := s.listRepos(s.dataMountPoint)
	if err != nil {
		s.log.Error("gc: failed to list repository directories", logger.ErrAttr(err))
		return
	}

	for _, repoDir := range repoDirs {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Each iteration releases the gate via defer so a panic in liveRevisions or
		// sweepRepoDir - which graceful.SafeGo recovers - cannot leave the exclusive
		// lock held, which would block every later deployment of this repository forever.
		if stop := func() bool {
			unlockGC, acquired, lockErr := sourcecache.TryAcquireExclusiveGCPathLock(repoDir)
			if lockErr != nil {
				s.log.Error("gc: failed to acquire repository GC lock; skipping repository",
					slog.String("repository", repoDir), logger.ErrAttr(lockErr))

				return false
			}

			if !acquired {
				return false
			}

			defer unlockGC()

			live, err := s.liveRevisions(ctx, s.contexts, s.log, s.dataMountSource, s.dataMountPoint)
			if err != nil {
				s.log.Error("gc: failed to discover all live revisions; skipping sweep", logger.ErrAttr(err))
				return true
			}

			s.sweepRepoDir(repoDir, live)

			return false
		}(); stop {
			return
		}
	}
}

// sweepRepoDir sweeps one repository/artifact directory, merging its
// label-derived live revisions with any revision currently in flight for it
// (see internal/source.IsInFlight) before delegating to store.Sweep.
func (s *Sweeper) sweepRepoDir(repoDir string, live map[string]set.Set[store.Revision]) {
	// The store's base directory is "<dataMountPoint>/<repoName>", where
	// repoName is the multi-segment "<host>/<owner>/<repo>" that Prepare
	// derives (internal/source/prepare.go) and marks in flight under, so the
	// key here has to be the path relative to the mount point - not its base
	// name, which is only the last segment.
	repoName, err := filepath.Rel(s.dataMountPoint, repoDir)
	if err != nil {
		s.log.Error("gc: failed to derive repository name", slog.String("dir", repoDir), logger.ErrAttr(err))
		return
	}

	repoName = filepath.ToSlash(repoName)
	repoLog := s.log.With(slog.String("repository", repoName))

	revisions := liveRevisionsFor(live, repoName)
	if revisions.Contains(allRevisions) {
		repoLog.Debug("gc: skipping repository referenced by deployment without config revision metadata")
		return
	}

	// An artifact that Prepare just published but whose deployment has not
	// finished (and so has not labeled anything) yet would otherwise look
	// unreferenced here - IsInFlight closes exactly that gap. Every artifact
	// under this directory is checked, not just those already in the label
	// -derived live set, since the only cheap identifier available here is
	// the artifact directory's own revision name.
	artifacts, err := store.ListArtifacts(repoDir)
	if err != nil {
		repoLog.Error("gc: failed to list artifacts", logger.ErrAttr(err))
		return
	}

	for _, artifact := range artifacts {
		if source.IsInFlight(repoName, string(artifact.Revision)) {
			revisions.Add(artifact.Revision)
		}
	}

	result, err := s.sweepDirectory(repoDir, revisions, s.opts, s.now())
	if err != nil {
		repoLog.Error("gc: sweep failed", logger.ErrAttr(err))
	}

	if len(result.Removed) > 0 {
		removed := make([]string, 0, len(result.Removed))
		for _, a := range result.Removed {
			removed = append(removed, string(a.Revision))
		}

		repoLog.Info("gc: removed unreferenced artifacts", slog.Any("revisions", removed))
	}
}

// liveRevisionsFor collects the live revisions recorded for the repository
// stored at repoName, the store base directory's path relative to the data
// mount point (e.g. "github.com/owner/repo").
//
// The live set is keyed by the normalized cd.doco.source.name label, which
// carries the provider's full name ("owner/repo") and so is usually the
// on-disk path minus its host segment. A live key is therefore accepted when
// it equals repoName or is a trailing path-segment match of it. Matching too
// eagerly only retains an artifact longer than necessary; the opposite
// mistake deletes the source tree of a running stack.
func liveRevisionsFor(live map[string]set.Set[store.Revision], repoName string) set.Set[store.Revision] {
	revisions := make(set.Set[store.Revision])

	for key, keyRevisions := range live {
		if !repositoryKeyMatches(repoName, key) {
			continue
		}

		for rev := range keyRevisions {
			revisions.Add(rev)
		}
	}

	return revisions
}

// repositoryKeyMatches reports whether a live-set key refers to the
// repository stored at repoName. Either side may be the shorter one: the
// label normally omits the host segment the on-disk path has, but a label
// value carrying a full clone URL normalizes to the longer form instead.
func repositoryKeyMatches(repoName, liveKey string) bool {
	if repoName == "" || liveKey == "" {
		return false
	}

	if repoName == liveKey {
		return true
	}

	return strings.HasSuffix(repoName, "/"+liveKey) || strings.HasSuffix(liveKey, "/"+repoName)
}

// maxRepoSearchDepth bounds how deep below the data mount point a repository
// root is looked for. Repository names are "<host>/<owner>/<repo>", one level
// deeper for nested groups (e.g. GitLab subgroups); the cap keeps a periodic
// sweep from walking the entire contents of whatever else lives on the data volume.
const maxRepoSearchDepth = 6

// repoRootMarkers are the subdirectory names that identify a directory as a
// source store base directory (see internal/source/store): the bare mirror
// clone of a Git source, and the published artifacts both source types write.
var repoRootMarkers = []string{"artifacts", "mirror"}

// listRepositoryDirs returns the absolute path of every source store base
// directory beneath dataMountPoint.
//
// A repository directory is not an immediate child of the mount point:
// git.GetRepoName (and the OCI equivalent) produce a "<host>/<owner>/<repo>"
// path, so roots are found by walking - the same way internal/migration
// locates legacy repository roots - and recognizing a directory that holds an
// "artifacts" or "mirror" subdirectory. Descent stops at a root, so artifact
// contents are never walked.
func listRepositoryDirs(dataMountPoint string) ([]string, error) {
	if _, err := os.Stat(dataMountPoint); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, err
	}

	var dirs []string

	err := filepath.WalkDir(dataMountPoint, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable subtree must not abort the sweep of the others.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}

			return nil //nolint:nilerr // a single unreadable entry is skipped, not fatal
		}

		if !d.IsDir() || path == dataMountPoint {
			return nil
		}

		if isRepoRoot(path) {
			dirs = append(dirs, path)
			return filepath.SkipDir
		}

		rel, err := filepath.Rel(dataMountPoint, path)
		if err != nil || strings.Count(filepath.ToSlash(rel), "/")+1 >= maxRepoSearchDepth {
			return filepath.SkipDir
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return dirs, nil
}

// isRepoRoot reports whether dir is a source store base directory.
func isRepoRoot(dir string) bool {
	for _, marker := range repoRootMarkers {
		if info, err := os.Stat(filepath.Join(dir, marker)); err == nil && info.IsDir() {
			return true
		}
	}

	return false
}
