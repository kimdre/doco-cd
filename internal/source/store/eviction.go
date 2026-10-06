package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

// evictedSubdirs are the caches of a store that source eviction removes, in the order they are moved to the
// store's tombstone. They can all be fetched or published again. Mutable live data (LiveSubdir) is durable and never
// evicted: it is only removed when its stack is destroyed or no longer uses it.
var evictedSubdirs = []string{MirrorSubdir, SubmodulesSubdir, ArtifactsSubdir}

// tombstoneManifest names the file in a tombstone that records the store it was evicted from.
const tombstoneManifest = "tombstone.json"

// removeAll is overridable in tests.
var removeAll = os.RemoveAll

// SourceLastUsed returns when the store at baseDir was last used: the later of the last use recorded by its GC gate
// (see sourcecache.LastSourceUse) and the newest entry of its artifacts directory, e.g. its latest publication.
func SourceLastUsed(baseDir string) (time.Time, error) {
	last, err := sourcecache.LastSourceUse(baseDir)
	if err != nil {
		return time.Time{}, err
	}

	entries, err := os.ReadDir(filepath.Join(baseDir, ArtifactsSubdir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, err
	}

	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			return time.Time{}, err
		}

		if info.ModTime().After(last) {
			last = info.ModTime()
		}
	}

	return last, nil
}

// SourceEvictionBlocker returns why the caches of the store at baseDir must not be evicted regardless of its use, or
// "" if nothing in its layout prevents it.
//
// Stores nested in one of the caches would be evicted with them, so a cache that ever had a store nested in it, or
// looks like it holds one, blocks eviction for good (see containsNestedStore).
func SourceEvictionBlocker(baseDir string) (string, error) {
	if _, err := os.Lstat(filepath.Join(baseDir, ".git")); err == nil {
		return "the store still uses the legacy repository layout", nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	for _, sub := range evictedSubdirs {
		nested, err := containsNestedStore(filepath.Join(baseDir, sub), sub == ArtifactsSubdir)
		if err != nil {
			return "", err
		}

		if nested {
			return fmt.Sprintf("another source store is nested in its %s directory", sub), nil
		}
	}

	return "", nil
}

// containsNestedStore reports whether another store is, or looks like it is, nested in dir, a cache directory of a
// store. Gates record the stores nested in dir (see sourcecache.HasNestedSourceStores), but stores that have not been
// used since then leave only their layout or lock files behind, so dir is searched for them as well.
//
// The search skips the items a cache is made of, verified by their structure or name: bare Git repositories (the
// mirror and the submodule mirrors) and, in an artifacts directory, the published revisions, temporary directories
// and Compose Git include stores. Their content is not a store, even where it looks like one, e.g. a repository
// containing a "mirror" directory. A cache directory that is not made of such items, e.g. a group of repositories
// that happens to be named like one, is searched in full.
func containsNestedStore(dir string, artifacts bool) (bool, error) {
	if nested, err := sourcecache.HasNestedSourceStores(dir); err != nil || nested {
		return nested, err
	}

	info, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}

		return false, err
	}

	if !info.IsDir() {
		return false, nil
	}

	nested := false

	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// Entries removed while searching, e.g. temporary directories, cannot hold a store anymore.
			if path != dir && errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		name := entry.Name()

		switch {
		case !entry.IsDir():
			nested = sourcecache.IsSourceGateLockFile(name)
		case isBareRepository(path):
			return fs.SkipDir
		case path == dir:
			// A store whose base directory is dir itself.
			nested = isRepoRoot(path)
		case artifacts && filepath.Dir(path) == dir && isArtifactsCacheItem(name):
			return fs.SkipDir
		default:
			nested = name == LiveSubdir || isRepoRoot(path)
		}

		if nested {
			return fs.SkipAll
		}

		return nil
	})

	return nested, err
}

// isArtifactsCacheItem reports whether name, an entry of an artifacts directory, is a directory the store itself
// creates there: a published revision (see artifactDirName), a temporary or set-aside one (see tempArtifactPrefix), or
// the Compose Git include stores (ComposeGitCacheSubdir).
func isArtifactsCacheItem(name string) bool {
	return name == ComposeGitCacheSubdir || strings.HasPrefix(name, tempArtifactPrefix) || isRevisionDirName(name)
}

// isRevisionDirName reports whether name is the directory name of a revision: a Git commit hash or an OCI digest,
// encoded by artifactDirName.
func isRevisionDirName(name string) bool {
	for _, algorithm := range [...]string{"sha256", "sha384", "sha512"} {
		if digest, found := strings.CutPrefix(name, algorithm+"-"); found {
			name = digest

			break
		}
	}

	if len(name) < 40 {
		return false
	}

	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// tombstoneRecord is the content of a tombstone's manifest.
type tombstoneRecord struct {
	Source    string    `json:"source"`
	EvictedAt time.Time `json:"evicted_at"`
}

// TombstoneSource moves the caches of the store at baseDir (mirror, submodules and artifacts) into a new tombstone in
// the tombstone namespace of dataDir (see sourcecache.TombstoneDirName), from where PurgeTombstones removes them.
// Renaming is atomic, so the store never holds partially removed caches, and a tombstone whose removal is interrupted
// or fails is removed by a later purge independently of the store.
//
// Lock files next to and directly in baseDir, its live data and stores nested in it are kept. baseDir itself is only
// removed if nothing else is left in it.
//
// The caller must hold the store's eviction lock (see sourcecache.TryAcquireSourceEvictionLock). TombstoneSource
// returns the tombstone's path, or "" if the store had no caches. If moving a cache fails, the caches moved before
// it stay in the tombstone and the others in the store, for a later eviction.
func TombstoneSource(dataDir, baseDir string, now time.Time) (string, error) {
	rel, err := filepath.Rel(dataDir, baseDir)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("source store %s is not below %s", baseDir, dataDir)
	}

	rel = filepath.ToSlash(rel)

	if first, _, _ := strings.Cut(rel, "/"); first == sourcecache.TombstoneDirName {
		return "", fmt.Errorf("%w: %s", sourcecache.ErrReservedSourcePath, baseDir)
	}

	var caches []string

	for _, sub := range evictedSubdirs {
		if _, err := os.Lstat(filepath.Join(baseDir, sub)); err == nil {
			caches = append(caches, sub)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}

	if len(caches) == 0 {
		return "", nil
	}

	trash, err := tombstoneDir(dataDir, true)
	if err != nil {
		return "", err
	}

	tomb, err := os.MkdirTemp(trash, now.UTC().Format("20060102T150405Z")+"-")
	if err != nil {
		return "", fmt.Errorf("create tombstone: %w", err)
	}

	record, err := json.Marshal(tombstoneRecord{Source: rel, EvictedAt: now.UTC()})
	if err != nil {
		return tomb, err
	}

	if err := os.WriteFile(filepath.Join(tomb, tombstoneManifest), record, 0o600); err != nil {
		return tomb, fmt.Errorf("write tombstone manifest: %w", err)
	}

	for _, sub := range caches {
		// A rename across file systems fails instead of copying, which would not be atomic.
		if err := os.Rename(filepath.Join(baseDir, sub), filepath.Join(tomb, sub)); err != nil {
			return tomb, fmt.Errorf("move %s of source store %s to tombstone: %w", sub, rel, err)
		}
	}

	// Lock files and live data keep a used store's base directory, which is fine.
	_ = os.Remove(baseDir)

	return tomb, nil
}

// PurgeTombstones removes every tombstone in the tombstone namespace of dataDir and returns how many it removed.
// Everything in the namespace is garbage, so a tombstone that cannot be removed is retried by the next purge.
func PurgeTombstones(dataDir string) (int, error) {
	trash, err := tombstoneDir(dataDir, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}

		return 0, err
	}

	entries, err := os.ReadDir(trash)
	if err != nil {
		return 0, fmt.Errorf("list tombstones: %w", err)
	}

	var (
		removed int
		errs    []error
	)

	for _, entry := range entries {
		if err := removeAll(filepath.Join(trash, entry.Name())); err != nil {
			errs = append(errs, fmt.Errorf("remove tombstone %s: %w", entry.Name(), err))
			continue
		}

		removed++
	}

	return removed, errors.Join(errs...)
}

// tombstoneDir returns the tombstone namespace of dataDir, creating it if create is set. It refuses anything but a
// real directory, so tombstones can never be moved or removed through a symbolic link.
func tombstoneDir(dataDir string, create bool) (string, error) {
	dir := filepath.Join(dataDir, sourcecache.TombstoneDirName)

	if create {
		if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("create tombstone directory: %w", err)
		}
	}

	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}

	if !info.IsDir() {
		return "", fmt.Errorf("tombstone directory %s is not a directory", dir)
	}

	return dir, nil
}
