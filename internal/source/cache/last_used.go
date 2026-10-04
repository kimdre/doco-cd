package cache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// lastUsedSuffix names the sibling file whose modification time records when a source store was last used.
// It lives next to the store, like its lock files, so it neither shows up as a store entry nor as a legacy leftover.
const lastUsedSuffix = ".last-used"

// IsStoreUseFile reports whether name is the name of a file that the use of a source store leaves next to it: the
// lock file of its GC gate (see AcquireSharedGCPathLock) or its last-used record.
func IsStoreUseFile(name string) bool {
	return strings.HasSuffix(name, gcLockSuffix+".lock") || strings.HasSuffix(name, lastUsedSuffix)
}

func lastUsedPath(path string) string {
	return canonicalLockKey(path + lastUsedSuffix)
}

// TouchLastUsed records that the source store at path is in use now.
// AcquireSharedGCPathLock calls it for every holder, so the artifact garbage collector can tell
// how long a store has been unused before it removes it.
func TouchLastUsed(path string) error {
	marker := lastUsedPath(path)
	now := time.Now()

	err := os.Chtimes(marker, now, now)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if err = os.MkdirAll(filepath.Dir(marker), 0o750); err != nil {
		return err
	}

	file, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY, 0o640) //nolint:gosec // marker path is derived from doco-cd's own resolved data paths
	if err != nil {
		return err
	}

	if err = file.Close(); err != nil {
		return err
	}

	return os.Chtimes(marker, now, now)
}

// LastUsed returns when the source store at path was last used, as recorded by TouchLastUsed.
// ok is false when no use has been recorded yet.
func LastUsed(path string) (lastUsed time.Time, ok bool, err error) {
	info, err := os.Stat(lastUsedPath(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, false, nil
		}

		return time.Time{}, false, err
	}

	return info.ModTime(), true, nil
}

// RemoveLastUsed deletes the last-used record of the source store at path.
func RemoveLastUsed(path string) error {
	err := os.Remove(lastUsedPath(path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}
