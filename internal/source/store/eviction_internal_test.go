package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

// Not parallel: it overrides removeAll.
func TestPurgeTombstones_RetriesFailedRemovals(t *testing.T) {
	dataDir := t.TempDir()

	app := filepath.Join(dataDir, "github.com", "owner", "app")
	if err := os.MkdirAll(filepath.Join(app, ArtifactsSubdir, "rev"), 0o750); err != nil {
		t.Fatal(err)
	}

	tomb, err := TombstoneSource(dataDir, app, time.Now())
	if err != nil {
		t.Fatalf("TombstoneSource() error = %v", err)
	}

	errRemove := errors.New("device busy")

	removeAll = func(string) error { return errRemove }

	t.Cleanup(func() { removeAll = os.RemoveAll })

	if removed, err := PurgeTombstones(dataDir); removed != 0 || !errors.Is(err, errRemove) {
		t.Fatalf("PurgeTombstones() = %d, %v, want the removal error", removed, err)
	}

	if _, err := os.Stat(filepath.Join(tomb, ArtifactsSubdir, "rev")); err != nil {
		t.Fatalf("failed purge lost the tombstone: %v", err)
	}

	// The store can be published and evicted again meanwhile.
	if err := os.MkdirAll(filepath.Join(app, ArtifactsSubdir, "rev"), 0o750); err != nil {
		t.Fatal(err)
	}

	if _, err := TombstoneSource(dataDir, app, time.Now()); err != nil {
		t.Fatalf("TombstoneSource() with a pending tombstone error = %v", err)
	}

	removeAll = os.RemoveAll

	if removed, err := PurgeTombstones(dataDir); removed != 2 || err != nil {
		t.Fatalf("PurgeTombstones() retry = %d, %v, want both tombstones removed", removed, err)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, sourcecache.TombstoneDirName))
	if err != nil || len(entries) != 0 {
		t.Fatalf("tombstone namespace = %v, %v, want it empty", entries, err)
	}
}
