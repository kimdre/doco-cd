package gc

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// sourceReferencesTimeout bounds how long an eviction pass inspects the Docker contexts while it holds the GC gates
// of the stores it is about to evict, which delays any deployment of them.
const sourceReferencesTimeout = 2 * time.Minute

// SourceSweeper periodically evicts the disposable caches of source stores that have not been used for longer than
// a retention TTL: their Git mirror, submodule mirrors and published artifacts (see store.TombstoneSource). All of
// them are fetched or published again by the next deployment of the source. Mutable live data is durable and never
// evicted.
//
// A store is only evicted if nothing on any configured Docker context references it, see sourceReferences, and
// while holding its eviction lock (see sourcecache.TryAcquireSourceEvictionLock), so no deployment, scheduled run,
// mirror compaction or store nested in it uses it meanwhile. Evicted caches are moved to the tombstone namespace
// atomically and removed from there, by the same pass or, after a crash or failed removal, by a later one.
type SourceSweeper struct {
	contexts        *docker.ContextRegistry
	log             *slog.Logger
	dataMountSource string
	dataMountPoint  string
	ttl             time.Duration
	interval        time.Duration

	// now, listRepos, lastUsed, blocker, tryLock, references, tombstone and purge are overridable in tests.
	now        func() time.Time
	listRepos  func(dataMountPoint string) ([]string, error)
	lastUsed   func(baseDir string) (time.Time, error)
	blocker    func(baseDir string) (string, error)
	tryLock    func(baseDir string) (func(), bool, error)
	references func(ctx context.Context) (*sourceReferences, error)
	tombstone  func(dataDir, baseDir string, now time.Time) (string, error)
	purge      func(dataDir string) (int, error)
}

// NewSourceSweeper creates a SourceSweeper for the source stores below the data directory, whose path is
// dataMountSource on the host and dataMountPoint in this container. It evicts the caches of stores unused for longer
// than ttl every interval.
func NewSourceSweeper(
	contexts *docker.ContextRegistry,
	log *slog.Logger,
	dataMountSource string,
	dataMountPoint string,
	ttl time.Duration,
	interval time.Duration,
) *SourceSweeper {
	s := &SourceSweeper{
		contexts:        contexts,
		log:             log.With(slog.String("component", "source-gc")),
		dataMountSource: dataMountSource,
		dataMountPoint:  dataMountPoint,
		ttl:             ttl,
		interval:        interval,
		now:             time.Now,
		listRepos:       store.ListRepositoryDirs,
		lastUsed:        store.SourceLastUsed,
		blocker:         store.SourceEvictionBlocker,
		tryLock:         sourcecache.TryAcquireSourceEvictionLock,
		tombstone:       store.TombstoneSource,
		purge:           store.PurgeTombstones,
	}

	s.references = func(ctx context.Context) (*sourceReferences, error) {
		return collectSourceReferences(ctx, s.contexts, s.dataMountSource, s.dataMountPoint)
	}

	return s
}

// PurgeSourceTombstones removes the tombstones of source stores evicted below dataMountPoint whose removal was
// interrupted, e.g. by a crash, or that are left over from before source eviction was disabled.
func PurgeSourceTombstones(log *slog.Logger, dataMountPoint string) {
	purgeTombstones(log.With(slog.String("component", "source-gc")), store.PurgeTombstones, dataMountPoint)
}

func purgeTombstones(log *slog.Logger, purge func(string) (int, error), dataMountPoint string) {
	removed, err := purge(dataMountPoint)
	if err != nil {
		log.Error("source gc: failed to remove evicted source data; retrying on the next pass", logger.ErrAttr(err))
	}

	if removed > 0 {
		log.Debug("source gc: removed evicted source data", slog.Int("tombstones", removed))
	}
}

// Start runs the sweeper loop until ctx is canceled. It removes leftover tombstones immediately, but only evicts
// stores after the first interval: stores from before eviction was enabled have not recorded their use yet, and
// every deployment and poll in the meantime gets the chance to do so. Callers are expected to run Start in its own
// goroutine, e.g. via graceful.SafeGo.
func (s *SourceSweeper) Start(ctx context.Context) {
	if s.log == nil || s.dataMountPoint == "" {
		return
	}

	s.log.Info("starting source garbage collector",
		slog.String("retention_ttl", s.ttl.String()),
		slog.String("interval", s.interval.String()),
	)

	purgeTombstones(s.log, s.purge, s.dataMountPoint)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("source garbage collector stopped")
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// evictionCandidate is a source store whose eviction lock is held.
type evictionCandidate struct {
	dir     string
	name    string
	release func()
}

// sweep evicts every unused, unreferenced source store it can lock.
func (s *SourceSweeper) sweep(ctx context.Context) {
	purgeTombstones(s.log, s.purge, s.dataMountPoint)

	var candidates []evictionCandidate

	// Each candidate's lock is released even if the pass panics, which graceful.SafeGo recovers: a gate left held
	// would block every later deployment of the store forever. lockCandidates adds each store as soon as it is locked,
	// so this covers a panic while it is still locking others.
	defer func() {
		for _, candidate := range candidates {
			candidate.release()
		}
	}()

	s.lockCandidates(ctx, &candidates)

	if len(candidates) == 0 {
		return
	}

	refsCtx, cancel := context.WithTimeout(ctx, sourceReferencesTimeout)
	refs, err := s.references(refsCtx)

	cancel()

	if err != nil {
		s.log.Error("source gc: failed to discover what uses the source stores; skipping eviction", logger.ErrAttr(err))
		return
	}

	evicted := 0

	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return
		}

		if s.evict(candidate, refs) {
			evicted++
		}

		// Release each store as soon as possible.
		candidate.release()
	}

	if evicted > 0 {
		purgeTombstones(s.log, s.purge, s.dataMountPoint)
	}
}

// lockCandidates adds the source stores that have been unused for longer than the TTL and nothing prevents from
// being evicted to candidates, each as soon as its eviction lock is held. Stores that are in use are skipped until the
// next pass.
func (s *SourceSweeper) lockCandidates(ctx context.Context, candidates *[]evictionCandidate) {
	repoDirs, err := s.listRepos(s.dataMountPoint)
	if err != nil {
		s.log.Error("source gc: failed to list source stores", logger.ErrAttr(err))
		return
	}

	for _, repoDir := range repoDirs {
		if ctx.Err() != nil {
			break
		}

		name, ok := s.sourceName(repoDir)
		if !ok {
			continue
		}

		repoLog := s.log.With(slog.String("source", name))

		if !s.evictable(repoLog, repoDir) {
			continue
		}

		release, acquired, err := s.tryLock(repoDir)
		if err != nil {
			repoLog.Error("source gc: failed to lock source store; skipping it", logger.ErrAttr(err))
			continue
		}

		if !acquired {
			repoLog.Debug("source gc: source store is in use; skipping it until the next pass")
			continue
		}

		*candidates = append(*candidates, evictionCandidate{dir: repoDir, name: name, release: sync.OnceFunc(release)})
	}
}

// sourceName returns the name of the store at repoDir, its path relative to the data directory, and whether it may
// be evicted at all.
func (s *SourceSweeper) sourceName(repoDir string) (string, bool) {
	rel, err := filepath.Rel(s.dataMountPoint, repoDir)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", false
	}

	name := filepath.ToSlash(rel)

	// Compose Git include stores outside a source store are shared by every deployment without a store of its own,
	// and do not record their use.
	if slices.Contains(strings.Split(name, "/"), store.ComposeGitCacheSubdir) {
		return "", false
	}

	return name, true
}

// evictable reports whether the store at repoDir has been unused for longer than the TTL and nothing in its layout
// prevents its eviction.
func (s *SourceSweeper) evictable(log *slog.Logger, repoDir string) bool {
	lastUsed, err := s.lastUsed(repoDir)
	if err != nil {
		log.Error("source gc: failed to determine when the source store was last used; skipping it", logger.ErrAttr(err))
		return false
	}

	if s.now().Sub(lastUsed) < s.ttl {
		return false
	}

	reason, err := s.blocker(repoDir)
	if err != nil {
		log.Error("source gc: failed to inspect source store; skipping it", logger.ErrAttr(err))
		return false
	}

	if reason != "" {
		log.Debug("source gc: keeping source store", slog.String("reason", reason))
		return false
	}

	return true
}

// evict moves the caches of a locked candidate to the tombstone namespace unless it is referenced or was used since
// it was selected, and reports whether it did.
func (s *SourceSweeper) evict(candidate evictionCandidate, refs *sourceReferences) bool {
	log := s.log.With(slog.String("source", candidate.name))

	// The store may have been used between its selection and locking.
	if !s.evictable(log, candidate.dir) {
		return false
	}

	if reference := refs.referencedBy(candidate.name); reference != "" {
		log.Debug("source gc: keeping source store referenced by a deployment", slog.String("reference", reference))
		return false
	}

	lastUsed, _ := s.lastUsed(candidate.dir)

	tomb, err := s.tombstone(s.dataMountPoint, candidate.dir, s.now())
	if err != nil {
		log.Error("source gc: failed to evict source store", logger.ErrAttr(err))

		// Caches already moved to the tombstone are removed regardless.
		return tomb != ""
	}

	if tomb == "" {
		return false
	}

	prometheus.SourceGCEvictedTotal.Inc()

	log.Info("source gc: evicted unused source store",
		slog.Time("last_used", lastUsed),
		slog.String("retention_ttl", s.ttl.String()))

	return true
}
