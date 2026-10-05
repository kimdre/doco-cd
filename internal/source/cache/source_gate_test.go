package cache_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

// setSourceRoot registers a new source root for the test. Tests using it must not run in parallel with each other.
func setSourceRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	t.Cleanup(sourcecache.SetSourceRoot(root))

	return root
}

func acquireEviction(t *testing.T, path string) func() {
	t.Helper()

	unlock, acquired, err := sourcecache.TryAcquireSourceEvictionLock(path)
	if err != nil {
		t.Fatalf("TryAcquireSourceEvictionLock(%s): %v", path, err)
	}

	if !acquired {
		t.Fatalf("expected eviction lock of %s to be available", path)
	}

	return unlock
}

func assertEvictionBusy(t *testing.T, path string) {
	t.Helper()

	unlock, acquired, err := sourcecache.TryAcquireSourceEvictionLock(path)
	if err != nil {
		t.Fatalf("TryAcquireSourceEvictionLock(%s): %v", path, err)
	}

	if acquired {
		unlock()
		t.Fatalf("expected eviction lock of %s to be busy", path)
	}
}

func TestSourceEviction_ExcludesNestedChildPreparation(t *testing.T) {
	root := setSourceRoot(t)
	parent := filepath.Join(root, "registry.example.com", "org", "app")
	child := filepath.Join(parent, "nested")

	unlockEviction := acquireEviction(t, parent)

	acquired := make(chan func(), 1)

	go func() {
		unlock, err := sourcecache.AcquireSharedGCPathLock(child)
		if err != nil {
			t.Errorf("AcquireSharedGCPathLock(child): %v", err)

			acquired <- func() {}

			return
		}

		acquired <- unlock
	}()

	select {
	case unlock := <-acquired:
		unlock()
		unlockEviction()
		t.Fatal("expected the nested child's gate to wait for the parent's eviction")
	case <-time.After(150 * time.Millisecond):
	}

	unlockEviction()

	select {
	case unlock := <-acquired:
		unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("expected the nested child's gate once the parent's eviction finished")
	}
}

func TestSourceEviction_BusyWhileStoreOrNestedChildIsUsed(t *testing.T) {
	root := setSourceRoot(t)
	parent := filepath.Join(root, "github.com", "org", "app")
	child := filepath.Join(parent, "nested")

	unlockChild, err := sourcecache.AcquireSharedGCPathLock(child)
	if err != nil {
		t.Fatal(err)
	}

	assertEvictionBusy(t, parent)
	unlockChild()

	unlockParent, err := sourcecache.AcquireSharedGCPathLock(parent)
	if err != nil {
		t.Fatal(err)
	}

	assertEvictionBusy(t, parent)
	unlockParent()

	acquireEviction(t, parent)()
}

func TestSourceEviction_ExcludesEvictionOfNestedStores(t *testing.T) {
	root := setSourceRoot(t)
	parent := filepath.Join(root, "github.com", "org", "app")
	child := filepath.Join(parent, "nested")

	unlockParent := acquireEviction(t, parent)
	assertEvictionBusy(t, child)
	unlockParent()

	unlockChild := acquireEviction(t, child)
	assertEvictionBusy(t, parent)
	unlockChild()
}

func TestSourceEviction_SiblingNamesDoNotCollide(t *testing.T) {
	root := setSourceRoot(t)
	app := filepath.Join(root, "github.com", "org", "app")
	sibling := filepath.Join(root, "github.com", "org", "app.evicting")

	unlockSibling, err := sourcecache.AcquireSharedGCPathLock(sibling)
	if err != nil {
		t.Fatal(err)
	}

	defer unlockSibling()

	acquireEviction(t, app)()
}

func TestSourceEviction_ExcludesOtherProcesses(t *testing.T) {
	root := setSourceRoot(t)
	parent := filepath.Join(root, "github.com", "org", "app")

	// Using a nested store creates the parent's tree lock file.
	unlock, err := sourcecache.AcquireSharedGCPathLock(filepath.Join(parent, "nested"))
	if err != nil {
		t.Fatal(err)
	}

	unlock()

	// A separate open file description conflicts like another process holding the tree lock.
	file, err := os.OpenFile(parent+".tree-use.lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = file.Close() }()

	if err := unix.Flock(int(file.Fd()), unix.LOCK_SH); err != nil {
		t.Fatal(err)
	}

	assertEvictionBusy(t, parent)

	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	acquireEviction(t, parent)()
}

func TestSourceGate_RejectsTombstoneNamespace(t *testing.T) {
	root := setSourceRoot(t)
	reserved := filepath.Join(root, sourcecache.TombstoneDirName, "github.com", "org", "app")

	if _, err := sourcecache.AcquireSharedGCPathLock(reserved); !errors.Is(err, sourcecache.ErrReservedSourcePath) {
		t.Fatalf("expected ErrReservedSourcePath, got %v", err)
	}

	if _, _, err := sourcecache.TryAcquireSourceEvictionLock(reserved); !errors.Is(err, sourcecache.ErrReservedSourcePath) {
		t.Fatalf("expected ErrReservedSourcePath, got %v", err)
	}

	if !sourcecache.IsReservedSourcePath(reserved) {
		t.Fatal("expected tombstone path to be reserved")
	}

	if sourcecache.IsReservedSourcePath(filepath.Join(root, "github.com", sourcecache.TombstoneDirName)) {
		t.Fatal("expected only the tombstone directory below the source root to be reserved")
	}
}

func TestIsReservedSourceName(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]bool{
		sourcecache.TombstoneDirName:                      true,
		sourcecache.TombstoneDirName + "/github.com/o/r":  true,
		"./" + sourcecache.TombstoneDirName + "/o/r":      true,
		"/" + sourcecache.TombstoneDirName + "/o/r":       true,
		"github.com/" + sourcecache.TombstoneDirName:      false,
		sourcecache.TombstoneDirName + "x/github.com/o/r": false,
		"github.com/owner/repo":                           false,
	} {
		if got := sourcecache.IsReservedSourceName(name); got != want {
			t.Errorf("IsReservedSourceName(%q) = %t, want %t", name, got, want)
		}
	}
}

func TestSourceEviction_RequiresStoreBelowSourceRoot(t *testing.T) {
	root := setSourceRoot(t)

	if _, _, err := sourcecache.TryAcquireSourceEvictionLock(root); err == nil {
		t.Fatal("expected the source root itself to be rejected")
	}

	if _, _, err := sourcecache.TryAcquireSourceEvictionLock(filepath.Join(t.TempDir(), "app")); err == nil {
		t.Fatal("expected a store outside the source root to be rejected")
	}

	restore := sourcecache.SetSourceRoot("")
	defer restore()

	if _, _, err := sourcecache.TryAcquireSourceEvictionLock(filepath.Join(root, "app")); !errors.Is(err, sourcecache.ErrSourceRootNotSet) {
		t.Fatalf("expected ErrSourceRootNotSet, got %v", err)
	}
}

func TestSourceGate_RecordsUse(t *testing.T) {
	root := setSourceRoot(t)
	store := filepath.Join(root, "github.com", "org", "app")

	if used, err := sourcecache.LastSourceUse(store); err != nil || !used.IsZero() {
		t.Fatalf("expected an unused store, got %v, %v", used, err)
	}

	unlock, err := sourcecache.AcquireSharedGCPathLock(store)
	if err != nil {
		t.Fatal(err)
	}

	unlock()

	lockFile := store + ".gc-use.lock"
	old := time.Now().Add(-48 * time.Hour)

	if err := os.Chtimes(lockFile, old, old); err != nil {
		t.Fatal(err)
	}

	unlock, err = sourcecache.AcquireSharedSourceMaintenanceLock(store)
	if err != nil {
		t.Fatal(err)
	}

	unlock()

	used, err := sourcecache.LastSourceUse(store)
	if err != nil {
		t.Fatal(err)
	}

	if !used.Equal(old) {
		t.Fatalf("expected maintenance not to record a use, last use %v", used)
	}

	unlock, err = sourcecache.AcquireSharedGCPathLock(store)
	if err != nil {
		t.Fatal(err)
	}

	unlock()

	used, err = sourcecache.LastSourceUse(store)
	if err != nil {
		t.Fatal(err)
	}

	if time.Since(used) > time.Minute {
		t.Fatalf("expected the gate to record a use, last use %v", used)
	}
}
