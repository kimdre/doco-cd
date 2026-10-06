package store_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/store"
)

const (
	testCommit = "0123456789abcdef0123456789abcdef01234567"
	testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func writeFile(t *testing.T, path string) {
	t.Helper()

	mkdirAll(t, filepath.Dir(path))

	if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertExists(t *testing.T, paths ...string) {
	t.Helper()

	for _, path := range paths {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("expected %s to exist: %v", path, err)
		}
	}
}

func assertMissing(t *testing.T, paths ...string) {
	t.Helper()

	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected %s to be removed, got %v", path, err)
		}
	}
}

func TestTombstoneSource_MovesOnlyCaches(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	app := filepath.Join(dataDir, "github.com", "owner", "app")

	makeBareMirror(t, filepath.Join(app, store.MirrorSubdir), "https://github.com/owner/app.git")
	makeBareMirror(t, filepath.Join(app, store.SubmodulesSubdir, "aaa"), "https://github.com/owner/lib.git")
	writeFile(t, filepath.Join(app, store.ArtifactsSubdir, "rev", "compose.yaml"))

	keep := []string{
		// Lock files next to and directly in the store.
		app + ".lock",
		app + ".gc-use.lock",
		app + ".tree-use.lock",
		filepath.Join(app, "mirror.lock"),
		// Durable live data.
		filepath.Join(app, store.LiveSubdir, "default", "stack", "manifest.json"),
		// A store nested in the base directory, and its lock files.
		filepath.Join(app, "nested", store.MirrorSubdir, "HEAD"),
		filepath.Join(app, "nested.gc-use.lock"),
		// A sibling whose name merely starts with the store's name.
		filepath.Join(dataDir, "github.com", "owner", "app.evicting", store.MirrorSubdir, "HEAD"),
		filepath.Join(dataDir, "github.com", "owner", "app.evicting.gc-use.lock"),
	}
	for _, path := range keep {
		writeFile(t, path)
	}

	writeFile(t, filepath.Join(app, store.SubmodulesSubdir, "aaa.lock"))
	writeFile(t, filepath.Join(app, store.ArtifactsSubdir, "rev.lock"))

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	tomb, err := store.TombstoneSource(dataDir, app, now)
	if err != nil {
		t.Fatalf("TombstoneSource() error = %v", err)
	}

	if filepath.Dir(tomb) != filepath.Join(dataDir, sourcecache.TombstoneDirName) {
		t.Fatalf("TombstoneSource() = %s, want a tombstone in the tombstone namespace", tomb)
	}

	assertMissing(t,
		filepath.Join(app, store.MirrorSubdir),
		filepath.Join(app, store.SubmodulesSubdir),
		filepath.Join(app, store.ArtifactsSubdir),
	)
	assertExists(t, keep...)
	assertExists(t,
		filepath.Join(tomb, store.MirrorSubdir, "HEAD"),
		filepath.Join(tomb, store.SubmodulesSubdir, "aaa.lock"),
		filepath.Join(tomb, store.ArtifactsSubdir, "rev", "compose.yaml"),
	)

	data, err := os.ReadFile(filepath.Join(tomb, "tombstone.json"))
	if err != nil {
		t.Fatal(err)
	}

	var record struct {
		Source    string    `json:"source"`
		EvictedAt time.Time `json:"evicted_at"`
	}
	if err := json.Unmarshal(data, &record); err != nil || record.Source != "github.com/owner/app" || !record.EvictedAt.Equal(now) {
		t.Fatalf("tombstone manifest = %s (%v), want source github.com/owner/app evicted at %v", data, err, now)
	}

	dirs, err := store.ListRepositoryDirs(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	slices.Sort(dirs)

	// The nested store was hidden by the evicted store's layout.
	want := []string{filepath.Join(app, "nested"), filepath.Join(dataDir, "github.com", "owner", "app.evicting")}
	slices.Sort(want)

	if !slices.Equal(dirs, want) {
		t.Fatalf("ListRepositoryDirs() = %v, want %v", dirs, want)
	}

	removed, err := store.PurgeTombstones(dataDir)
	if err != nil || removed != 1 {
		t.Fatalf("PurgeTombstones() = %d, %v, want 1 removed", removed, err)
	}

	assertMissing(t, tomb)
	assertExists(t, keep...)
}

func TestTombstoneSource_RemovesEmptyBaseDirOnly(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	image := filepath.Join(dataDir, "ghcr.io", "owner", "image")
	writeFile(t, filepath.Join(image, store.ArtifactsSubdir, "sha256-abc", "compose.yaml"))
	writeFile(t, image+".gc-use.lock")

	if _, err := store.TombstoneSource(dataDir, image, time.Now()); err != nil {
		t.Fatalf("TombstoneSource() error = %v", err)
	}

	assertMissing(t, image)
	assertExists(t, image+".gc-use.lock")

	// A store without caches has nothing to evict.
	empty := filepath.Join(dataDir, "ghcr.io", "owner", "empty")
	writeFile(t, filepath.Join(empty, store.LiveSubdir, "default", "stack", "manifest.json"))

	tomb, err := store.TombstoneSource(dataDir, empty, time.Now())
	if err != nil || tomb != "" {
		t.Fatalf("TombstoneSource(no caches) = %q, %v, want nothing to evict", tomb, err)
	}

	assertExists(t, filepath.Join(empty, store.LiveSubdir, "default", "stack", "manifest.json"))
}

func TestTombstoneSource_RejectsPathsOutsideTheStores(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()

	for _, baseDir := range []string{
		dataDir,
		filepath.Dir(dataDir),
		filepath.Join(t.TempDir(), "github.com", "owner", "app"),
		filepath.Join(dataDir, sourcecache.TombstoneDirName, "github.com", "owner", "app"),
	} {
		mkdirAll(t, filepath.Join(baseDir, store.ArtifactsSubdir))

		if _, err := store.TombstoneSource(dataDir, baseDir, time.Now()); err == nil {
			t.Errorf("TombstoneSource(%s) succeeded, want an error", baseDir)
		}

		assertExists(t, filepath.Join(baseDir, store.ArtifactsSubdir))
	}
}

func TestTombstones_RefuseSymlinkedNamespace(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	elsewhere := t.TempDir()
	writeFile(t, filepath.Join(elsewhere, "precious", "data"))

	if err := os.Symlink(elsewhere, filepath.Join(dataDir, sourcecache.TombstoneDirName)); err != nil {
		t.Fatal(err)
	}

	if _, err := store.PurgeTombstones(dataDir); err == nil {
		t.Fatal("PurgeTombstones() succeeded through a symbolic link")
	}

	app := filepath.Join(dataDir, "github.com", "owner", "app")
	writeFile(t, filepath.Join(app, store.ArtifactsSubdir, "rev", "compose.yaml"))

	if _, err := store.TombstoneSource(dataDir, app, time.Now()); err == nil {
		t.Fatal("TombstoneSource() succeeded through a symbolic link")
	}

	assertExists(t, filepath.Join(elsewhere, "precious", "data"), filepath.Join(app, store.ArtifactsSubdir, "rev", "compose.yaml"))
}

func TestPurgeTombstones_WithoutNamespace(t *testing.T) {
	t.Parallel()

	if removed, err := store.PurgeTombstones(t.TempDir()); removed != 0 || err != nil {
		t.Fatalf("PurgeTombstones() = %d, %v, want nothing to purge", removed, err)
	}
}

func TestSourceEvictionBlocker(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, app string)
		blocked bool
	}{
		{
			name: "unblocked",
			setup: func(t *testing.T, app string) {
				t.Helper()
				makeBareMirror(t, filepath.Join(app, store.MirrorSubdir), "https://github.com/owner/app.git")
				makeBareMirror(t, filepath.Join(app, store.SubmodulesSubdir, testCommit), "https://github.com/owner/lib.git")
				writeFile(t, filepath.Join(app, store.SubmodulesSubdir, testCommit+".lock"))
				writeFile(t, filepath.Join(app, store.ArtifactsSubdir, testCommit, "compose.yaml"))
				writeFile(t, filepath.Join(app, store.ArtifactsSubdir, testCommit+".lock"))
				writeFile(t, filepath.Join(app, store.ArtifactsSubdir, testCommit+".published"))
				writeFile(t, filepath.Join(app, store.ArtifactsSubdir, "unknown", "file"))
				writeFile(t, filepath.Join(app, "nested.gc-use.lock"))
			},
		},
		{
			name: "cache items that look like stores",
			setup: func(t *testing.T, app string) {
				t.Helper()
				// Bare repositories with branches named like store directories.
				makeBareMirror(t, filepath.Join(app, store.MirrorSubdir), "https://github.com/owner/app.git")
				mkdirAll(t, filepath.Join(app, store.MirrorSubdir, "refs", "heads", store.MirrorSubdir))
				mkdirAll(t, filepath.Join(app, store.MirrorSubdir, "refs", "heads", store.LiveSubdir))
				makeBareMirror(t, filepath.Join(app, store.SubmodulesSubdir, testCommit), "https://github.com/owner/lib.git")
				mkdirAll(t, filepath.Join(app, store.SubmodulesSubdir, testCommit, "refs", "heads", store.ArtifactsSubdir))
				// Publications of repositories with directories named like store directories.
				writeFile(t, filepath.Join(app, store.ArtifactsSubdir, testCommit, store.ArtifactsSubdir, store.MirrorSubdir, "file"))
				writeFile(t, filepath.Join(app, store.ArtifactsSubdir, "sha256-"+testDigest, store.LiveSubdir, "file"))
				writeFile(t, filepath.Join(app, store.ArtifactsSubdir, ".tmp-unpublished-"+testCommit+"-1", store.MirrorSubdir, "file"))
				writeFile(t, filepath.Join(app, store.ArtifactsSubdir, store.ComposeGitCacheSubdir, "aaa", store.MirrorSubdir, "HEAD"))
				writeFile(t, filepath.Join(app, store.ArtifactsSubdir, store.ComposeGitCacheSubdir, "aaa", store.LiveSubdir, "file"))
			},
		},
		{
			name: "legacy layout",
			setup: func(t *testing.T, app string) {
				t.Helper()
				writeFile(t, filepath.Join(app, ".git", "HEAD"))
			},
			blocked: true,
		},
		{
			name: "store nested in artifacts",
			setup: func(t *testing.T, app string) {
				t.Helper()
				writeFile(t, filepath.Join(app, store.ArtifactsSubdir+".tree-use.lock"))
			},
			blocked: true,
		},
		{
			name: "store gate in submodules",
			setup: func(t *testing.T, app string) {
				t.Helper()
				writeFile(t, filepath.Join(app, store.SubmodulesSubdir, "x.gc-use.lock"))
			},
			blocked: true,
		},
		{
			name: "store at the mirror directory",
			setup: func(t *testing.T, app string) {
				t.Helper()
				mkdirAll(t, filepath.Join(app, store.MirrorSubdir, store.ArtifactsSubdir))
			},
			blocked: true,
		},
		{
			// A group named like a cache directory, e.g. for the repository "github.com/owner/app/mirror/team/web",
			// makes its parent look like a store.
			name: "group named like the mirror",
			setup: func(t *testing.T, app string) {
				t.Helper()

				web := filepath.Join(app, store.MirrorSubdir, "team", "web")
				makeBareMirror(t, filepath.Join(web, store.MirrorSubdir), "https://github.com/owner/app/mirror/team/web.git")
				writeFile(t, filepath.Join(web, store.LiveSubdir, "data.db"))
			},
			blocked: true,
		},
		{
			name: "group named like the artifacts",
			setup: func(t *testing.T, app string) {
				t.Helper()
				writeFile(t, filepath.Join(app, store.ArtifactsSubdir, "team", "web", store.ArtifactsSubdir, testCommit, "compose.yaml"))
			},
			blocked: true,
		},
		{
			name: "nested store with only live data",
			setup: func(t *testing.T, app string) {
				t.Helper()
				writeFile(t, filepath.Join(app, store.MirrorSubdir, "team", "web", store.LiveSubdir, "data.db"))
			},
			blocked: true,
		},
		{
			name: "nested store with only its lock files",
			setup: func(t *testing.T, app string) {
				t.Helper()
				writeFile(t, filepath.Join(app, store.SubmodulesSubdir, "team", "web.gc-use.lock"))
			},
			blocked: true,
		},
		{
			name: "store layout in an unknown artifacts entry",
			setup: func(t *testing.T, app string) {
				t.Helper()
				mkdirAll(t, filepath.Join(app, store.ArtifactsSubdir, "unknown", store.MirrorSubdir))
			},
			blocked: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := filepath.Join(t.TempDir(), "github.com", "owner", "app")
			tc.setup(t, app)

			reason, err := store.SourceEvictionBlocker(app)
			if err != nil {
				t.Fatalf("SourceEvictionBlocker() error = %v", err)
			}

			if blocked := reason != ""; blocked != tc.blocked {
				t.Fatalf("SourceEvictionBlocker() = %q, want blocked %v", reason, tc.blocked)
			}
		})
	}
}

func TestSourceLastUsed(t *testing.T) {
	t.Parallel()

	app := filepath.Join(t.TempDir(), "github.com", "owner", "app")
	mkdirAll(t, app)

	if used, err := store.SourceLastUsed(app); err != nil || !used.IsZero() {
		t.Fatalf("SourceLastUsed(unused) = %v, %v, want zero", used, err)
	}

	gateUse := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	publication := gateUse.Add(24 * time.Hour)

	writeFile(t, app+".gc-use.lock")

	if err := os.Chtimes(app+".gc-use.lock", gateUse, gateUse); err != nil {
		t.Fatal(err)
	}

	if used, err := store.SourceLastUsed(app); err != nil || !used.Equal(gateUse) {
		t.Fatalf("SourceLastUsed() = %v, %v, want the gate's last use %v", used, err, gateUse)
	}

	published := filepath.Join(app, store.ArtifactsSubdir, "rev.published-at")
	writeFile(t, published)

	if err := os.Chtimes(published, publication, publication); err != nil {
		t.Fatal(err)
	}

	if used, err := store.SourceLastUsed(app); err != nil || !used.Equal(publication) {
		t.Fatalf("SourceLastUsed() = %v, %v, want the latest publication %v", used, err, publication)
	}
}
