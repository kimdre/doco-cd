package gc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kimdre/doco-cd/internal/prometheus"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/store"
)

const testSourceTTL = 24 * time.Hour

// testSourceLocks is a fake for TryAcquireSourceEvictionLock that grants every lock except those of busy or failing
// stores, and counts the releases of each granted lock.
type testSourceLocks struct {
	mu       sync.Mutex
	busy     map[string]bool
	failing  map[string]bool
	onLock   func(dir string)
	acquired map[string]int
	released map[string]int
}

func newTestSourceLocks() *testSourceLocks {
	return &testSourceLocks{
		busy:     make(map[string]bool),
		failing:  make(map[string]bool),
		acquired: make(map[string]int),
		released: make(map[string]int),
	}
}

func (l *testSourceLocks) tryLock(dir string) (func(), bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch {
	case l.failing[dir]:
		return nil, false, errors.New("lock failed")
	case l.busy[dir]:
		return nil, false, nil
	}

	if l.onLock != nil {
		l.onLock(dir)
	}

	l.acquired[dir]++

	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()

		l.released[dir]++
	}, true, nil
}

func (l *testSourceLocks) assertAllReleasedOnce(t *testing.T) {
	t.Helper()

	l.mu.Lock()
	defer l.mu.Unlock()

	for dir, acquired := range l.acquired {
		if released := l.released[dir]; released != acquired {
			t.Errorf("lock of %s acquired %d times, released %d times", dir, acquired, released)
		}
	}
}

func newTestSourceSweeper(t *testing.T) (*SourceSweeper, *testSourceLocks) {
	t.Helper()

	dataMountPoint := t.TempDir()
	locks := newTestSourceLocks()

	s := NewSourceSweeper(nil, testLogger(), "/srv/doco-cd/data", dataMountPoint, testSourceTTL, time.Hour)
	s.tryLock = locks.tryLock
	s.references = func(context.Context) (*sourceReferences, error) {
		return newSourceReferences(s.dataMountSource, s.dataMountPoint), nil
	}

	return s, locks
}

// makeSourceStore creates a store with a mirror, submodule mirrors, live data and an artifact last used at lastUsed
// below dataDir, and returns its base directory.
func makeSourceStore(t *testing.T, dataDir, name string, lastUsed time.Time) string {
	t.Helper()

	baseDir := filepath.Join(dataDir, filepath.FromSlash(name))

	for _, file := range []string{
		filepath.Join(store.MirrorSubdir, "HEAD"),
		filepath.Join(store.SubmodulesSubdir, "abc", "HEAD"),
		filepath.Join(store.LiveSubdir, "data.db"),
	} {
		writeTestFile(t, filepath.Join(baseDir, file))
	}

	artifact := makeArtifactDir(t, baseDir, "rev")
	writeTestFile(t, filepath.Join(artifact, "compose.yaml"))

	if err := os.Chtimes(artifact, lastUsed, lastUsed); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	return baseDir
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertEvicted(t *testing.T, baseDir string) {
	t.Helper()

	for _, sub := range []string{store.MirrorSubdir, store.SubmodulesSubdir, store.ArtifactsSubdir} {
		if _, err := os.Lstat(filepath.Join(baseDir, sub)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s of %s was not evicted: %v", sub, baseDir, err)
		}
	}

	if _, err := os.Stat(filepath.Join(baseDir, store.LiveSubdir, "data.db")); err != nil {
		t.Errorf("live data of %s was not kept: %v", baseDir, err)
	}
}

func assertKept(t *testing.T, baseDir string) {
	t.Helper()

	for _, path := range []string{
		filepath.Join(store.MirrorSubdir, "HEAD"),
		filepath.Join(store.ArtifactsSubdir, "rev", "compose.yaml"),
		filepath.Join(store.LiveSubdir, "data.db"),
	} {
		if _, err := os.Stat(filepath.Join(baseDir, path)); err != nil {
			t.Errorf("%s of %s was not kept: %v", path, baseDir, err)
		}
	}
}

func assertNoTombstones(t *testing.T, dataDir string) {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(dataDir, sourcecache.TombstoneDirName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read tombstones: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("tombstones were not removed: %v", entries)
	}
}

func TestSourceSweeper_Sweep(t *testing.T) {
	t.Parallel()

	s, locks := newTestSourceSweeper(t)
	dataDir := s.dataMountPoint
	old := time.Now().Add(-2 * testSourceTTL)
	recent := time.Now().Add(-time.Hour)

	unused := makeSourceStore(t, dataDir, "github.com/owner/unused", old)
	used := makeSourceStore(t, dataDir, "github.com/owner/used", recent)
	referenced := makeSourceStore(t, dataDir, "github.com/owner/referenced", old)
	busy := makeSourceStore(t, dataDir, "github.com/owner/busy", old)
	failing := makeSourceStore(t, dataDir, "github.com/owner/failing", old)
	legacy := makeSourceStore(t, dataDir, "github.com/owner/legacy", old)
	include := makeSourceStore(t, dataDir, store.ComposeGitCacheSubdir+"/0123abcd", old)

	writeTestFile(t, filepath.Join(legacy, ".git", "HEAD"))

	locks.busy[busy] = true
	locks.failing[failing] = true

	s.references = func(context.Context) (*sourceReferences, error) {
		refs := newSourceReferences(s.dataMountSource, s.dataMountPoint)
		refs.addPath(s.dataMountSource + "/github.com/owner/referenced/artifacts/rev")

		return refs, nil
	}

	s.sweep(t.Context())

	assertEvicted(t, unused)

	for _, kept := range []string{used, referenced, busy, failing, legacy, include} {
		assertKept(t, kept)
	}

	assertNoTombstones(t, dataDir)
	locks.assertAllReleasedOnce(t)

	if locks.acquired[used] != 0 || locks.acquired[legacy] != 0 || locks.acquired[include] != 0 {
		t.Errorf("locked stores that are not evictable: %v", locks.acquired)
	}
}

func TestSourceSweeper_Sweep_RemovesEmptyStore(t *testing.T) {
	t.Parallel()

	s, _ := newTestSourceSweeper(t)

	// A store that only ever held a mirror and has not recorded any use.
	mirrorOnly := filepath.Join(s.dataMountPoint, "github.com", "owner", "mirror-only")
	writeTestFile(t, filepath.Join(mirrorOnly, store.MirrorSubdir, "HEAD"))

	s.sweep(t.Context())

	if _, err := os.Lstat(mirrorOnly); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("empty store %s was not removed: %v", mirrorOnly, err)
	}
}

func TestSourceSweeper_Sweep_KeepsStoreUsedWhileLocking(t *testing.T) {
	t.Parallel()

	s, locks := newTestSourceSweeper(t)
	baseDir := makeSourceStore(t, s.dataMountPoint, "github.com/owner/app", time.Now().Add(-2*testSourceTTL))

	// A deployment uses the store between its selection and locking.
	locks.onLock = func(dir string) {
		now := time.Now()
		if err := os.Chtimes(filepath.Join(dir, store.ArtifactsSubdir, "rev"), now, now); err != nil {
			t.Errorf("Chtimes: %v", err)
		}
	}

	s.sweep(t.Context())

	assertKept(t, baseDir)
	locks.assertAllReleasedOnce(t)
}

func TestSourceSweeper_Sweep_EvictsNothingWithoutReferences(t *testing.T) {
	t.Parallel()

	s, locks := newTestSourceSweeper(t)
	baseDir := makeSourceStore(t, s.dataMountPoint, "github.com/owner/app", time.Now().Add(-2*testSourceTTL))

	s.references = func(context.Context) (*sourceReferences, error) {
		return nil, errors.New("docker unavailable")
	}

	s.sweep(t.Context())

	assertKept(t, baseDir)

	if locks.acquired[baseDir] != 1 {
		t.Fatalf("store was locked %d times, want 1", locks.acquired[baseDir])
	}

	locks.assertAllReleasedOnce(t)
}

func TestSourceSweeper_Sweep_ReleasesLocksOnPanic(t *testing.T) {
	t.Parallel()

	s, locks := newTestSourceSweeper(t)
	first := makeSourceStore(t, s.dataMountPoint, "github.com/owner/first", time.Now().Add(-2*testSourceTTL))
	second := makeSourceStore(t, s.dataMountPoint, "github.com/owner/second", time.Now().Add(-2*testSourceTTL))

	s.tombstone = func(string, string, time.Time) (string, error) {
		panic("tombstone failed")
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("sweep() did not panic")
			}
		}()

		s.sweep(t.Context())
	}()

	assertKept(t, first)
	assertKept(t, second)

	if len(locks.acquired) != 2 {
		t.Fatalf("locked stores = %v, want both", locks.acquired)
	}

	locks.assertAllReleasedOnce(t)
}

func TestSourceSweeper_Sweep_ReleasesLocksOnPanicWhileLocking(t *testing.T) {
	t.Parallel()

	s, locks := newTestSourceSweeper(t)
	first := makeSourceStore(t, s.dataMountPoint, "github.com/owner/first", time.Now().Add(-2*testSourceTTL))
	second := makeSourceStore(t, s.dataMountPoint, "github.com/owner/second", time.Now().Add(-2*testSourceTTL))

	attempts := 0
	locks.onLock = func(string) {
		if attempts++; attempts == 2 {
			panic("lock failed")
		}
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("sweep() did not panic")
			}
		}()

		s.sweep(t.Context())
	}()

	assertKept(t, first)
	assertKept(t, second)

	if len(locks.acquired) != 1 {
		t.Fatalf("locked stores = %v, want one", locks.acquired)
	}

	locks.assertAllReleasedOnce(t)
}

func TestSourceSweeper_Sweep_StopsOnContextCancellation(t *testing.T) {
	t.Parallel()

	s, locks := newTestSourceSweeper(t)
	baseDir := makeSourceStore(t, s.dataMountPoint, "github.com/owner/app", time.Now().Add(-2*testSourceTTL))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	s.sweep(ctx)

	assertKept(t, baseDir)
	locks.assertAllReleasedOnce(t)
}

func TestSourceSweeper_Sweep_EvictsDespiteFailedPurge(t *testing.T) {
	t.Parallel()

	s, _ := newTestSourceSweeper(t)
	baseDir := makeSourceStore(t, s.dataMountPoint, "github.com/owner/app", time.Now().Add(-2*testSourceTTL))

	purges := 0
	s.purge = func(dataDir string) (int, error) {
		purges++
		if purges == 1 {
			return 0, errors.New("purge failed")
		}

		return store.PurgeTombstones(dataDir)
	}

	s.sweep(t.Context())

	assertEvicted(t, baseDir)
	assertNoTombstones(t, s.dataMountPoint)

	if purges != 2 {
		t.Errorf("purged %d times, want before and after evicting", purges)
	}
}

func TestSourceSweeper_Start_PurgesLeftoverTombstones(t *testing.T) {
	t.Parallel()

	s, locks := newTestSourceSweeper(t)
	baseDir := makeSourceStore(t, s.dataMountPoint, "github.com/owner/app", time.Now().Add(-2*testSourceTTL))
	writeTestFile(t, filepath.Join(s.dataMountPoint, sourcecache.TombstoneDirName, "20260101T000000Z-1", store.MirrorSubdir, "HEAD"))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	s.Start(ctx)

	assertNoTombstones(t, s.dataMountPoint)

	// Stores are only evicted from the first interval on.
	assertKept(t, baseDir)

	if len(locks.acquired) != 0 {
		t.Errorf("locked stores on start: %v", locks.acquired)
	}
}

func TestPurgeSourceTombstones(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	writeTestFile(t, filepath.Join(dataDir, sourcecache.TombstoneDirName, "20260101T000000Z-1", store.MirrorSubdir, "HEAD"))

	PurgeSourceTombstones(testLogger(), dataDir)

	assertNoTombstones(t, dataDir)

	// Nothing to purge.
	PurgeSourceTombstones(testLogger(), t.TempDir())
}

// TestSourceSweeper_Sweep_SourceGates evicts stores with the real source gates. It registers the global source root,
// so it must not run in parallel.
func TestSourceSweeper_Sweep_SourceGates(t *testing.T) {
	s, _ := newTestSourceSweeper(t)
	dataDir := s.dataMountPoint
	t.Cleanup(sourcecache.SetSourceRoot(dataDir))

	s.tryLock = sourcecache.TryAcquireSourceEvictionLock
	// Every store is unused for longer than the TTL, even after its gate records a use.
	s.now = func() time.Time { return time.Now().Add(2 * testSourceTTL) }

	app := makeSourceStore(t, dataDir, "github.com/owner/app", time.Now())
	parent := makeSourceStore(t, dataDir, "gitlab.com/group/parent", time.Now())
	child := makeSourceStore(t, dataDir, "gitlab.com/group/parent/child", time.Now())
	cached := makeSourceStore(t, dataDir, "gitlab.com/group/cached", time.Now())
	nested := makeSourceStore(t, dataDir, "gitlab.com/group/cached/artifacts/nested", time.Now())

	evictedBefore := testutil.ToFloat64(prometheus.SourceGCEvictedTotal)

	// Stores in use by a deployment, or with a store in use nested in them, are skipped.
	releaseApp, err := sourcecache.AcquireSharedGCPathLock(app)
	if err != nil {
		t.Fatal(err)
	}

	releaseChild, err := sourcecache.AcquireSharedGCPathLock(child)
	if err != nil {
		t.Fatal(err)
	}

	releaseNested, err := sourcecache.AcquireSharedGCPathLock(nested)
	if err != nil {
		t.Fatal(err)
	}

	s.sweep(t.Context())

	for _, kept := range []string{app, parent, child, cached, nested} {
		assertKept(t, kept)
	}

	releaseApp()
	releaseChild()
	releaseNested()

	s.sweep(t.Context())

	// The caches of a store are evicted without the stores nested in it, which are evicted on a later pass.
	assertEvicted(t, app)
	assertEvicted(t, parent)
	assertKept(t, child)

	// A store nested in the caches of another one would be evicted with them, so the other one is kept for good.
	assertKept(t, cached)
	assertKept(t, nested)

	if got := testutil.ToFloat64(prometheus.SourceGCEvictedTotal) - evictedBefore; got != 2 {
		t.Errorf("source_gc_evicted_total increased by %v, want 2", got)
	}

	s.sweep(t.Context())

	assertEvicted(t, child)
	assertKept(t, cached)

	// Evicted stores are deployable again.
	release, err := sourcecache.AcquireSharedGCPathLock(app)
	if err != nil {
		t.Fatalf("AcquireSharedGCPathLock() after eviction error = %v", err)
	}

	release()
}
