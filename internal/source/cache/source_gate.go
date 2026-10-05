package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// TombstoneDirName is the directory directly below the source root that holds the tombstones of evicted source
// stores until they are purged (see internal/source/store). It is reserved: no source store may live in it, and
// everything in it is garbage.
const TombstoneDirName = ".evicted"

// treeLockSuffix names the lock that coordinates a source store with the stores nested below it. Every gate
// acquisition holds it shared for each directory its store is nested in, and source eviction holds it exclusively
// for the evicted store, so a store cannot be prepared below a store that is being evicted.
const treeLockSuffix = ".tree-use"

// ErrReservedSourcePath reports a source path in the tombstone namespace.
var ErrReservedSourcePath = errors.New("path is reserved for evicted source data")

// ErrSourceRootNotSet reports a source eviction without a registered source root.
var ErrSourceRootNotSet = errors.New("source root is not set")

var sourceRoot struct {
	mu   sync.RWMutex
	path string
}

// SetSourceRoot registers root, the data directory holding all source stores. Below it, source gates also
// coordinate with the stores their store is nested in, and the tombstone namespace is reserved. It returns a
// function restoring the previous root.
func SetSourceRoot(root string) func() {
	if root != "" {
		root = canonicalLockKey(root)
	}

	sourceRoot.mu.Lock()
	previous := sourceRoot.path
	sourceRoot.path = root
	sourceRoot.mu.Unlock()

	return func() {
		sourceRoot.mu.Lock()
		sourceRoot.path = previous
		sourceRoot.mu.Unlock()
	}
}

func currentSourceRoot() string {
	sourceRoot.mu.RLock()
	defer sourceRoot.mu.RUnlock()

	return sourceRoot.path
}

// sourceRelPath returns the slash-separated path of path relative to the source root. ok is false if no root is
// registered or path is not strictly below it.
func sourceRelPath(path string) (root, rel string, ok bool) {
	root = currentSourceRoot()
	if root == "" {
		return "", "", false
	}

	rel, err := filepath.Rel(root, canonicalLockKey(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return root, "", false
	}

	return root, filepath.ToSlash(rel), true
}

// IsReservedSourcePath reports whether path is in the tombstone namespace below the source root.
func IsReservedSourcePath(path string) bool {
	_, rel, ok := sourceRelPath(path)

	return ok && isReservedRelPath(rel)
}

// IsReservedSourceName reports whether name, the path of a source store relative to the source root, is in the
// tombstone namespace.
func IsReservedSourceName(name string) bool {
	return isReservedRelPath(strings.TrimLeft(filepath.ToSlash(filepath.Clean(string(filepath.Separator)+name)), "/"))
}

func isReservedRelPath(rel string) bool {
	first, _, _ := strings.Cut(rel, "/")

	return first == TombstoneDirName
}

// sourceAncestors returns the directories below the source root that the store at path is nested in, outermost
// first. Paths outside the source root have none.
func sourceAncestors(path string) ([]string, error) {
	root, rel, ok := sourceRelPath(path)
	if !ok {
		return nil, nil
	}

	if isReservedRelPath(rel) {
		return nil, fmt.Errorf("%w: %s", ErrReservedSourcePath, path)
	}

	parts := strings.Split(rel, "/")
	ancestors := make([]string, 0, len(parts)-1)

	for i := 1; i < len(parts); i++ {
		ancestors = append(ancestors, filepath.Join(root, filepath.FromSlash(strings.Join(parts[:i], "/"))))
	}

	return ancestors, nil
}

func treeLockKey(dir string) string {
	return canonicalLockKey(canonicalLockKey(dir) + treeLockSuffix)
}

func treeLockMutex(key string) *sync.RWMutex {
	value, _ := repoLocks.LoadOrStore(key, &sync.RWMutex{})

	return value.(*sync.RWMutex)
}

// acquireSharedTreeLocks holds the tree lock of every directory in dirs shared, in order.
func acquireSharedTreeLocks(dirs []string) (func(), error) {
	releases := make([]func(), 0, len(dirs))

	release := func() {
		for _, r := range slices.Backward(releases) {
			r()
		}
	}

	for _, dir := range dirs {
		key := treeLockKey(dir)
		mutex := treeLockMutex(key)
		mutex.RLock()

		unlockFile, err := acquireRequiredCrossProcessLock(key, unix.LOCK_SH)
		if err != nil {
			mutex.RUnlock()
			release()

			return nil, err
		}

		releases = append(releases, func() {
			unlockFile()
			mutex.RUnlock()
		})
	}

	return release, nil
}

// TryAcquireSourceEvictionLock attempts to take everything needed to remove the source store at path, without
// waiting: its GC gate exclusively, so no deployment, scheduled run or artifact sweep uses it, its tree lock
// exclusively, so no store can be prepared below it, and the tree locks of the stores it is nested in shared, so none
// of them is evicted at the same time. It returns false if any of them is held.
//
// path must be strictly below the source root (see SetSourceRoot) and outside the tombstone namespace.
func TryAcquireSourceEvictionLock(path string) (func(), bool, error) {
	if _, _, ok := sourceRelPath(path); !ok {
		if currentSourceRoot() == "" {
			return nil, false, ErrSourceRootNotSet
		}

		return nil, false, fmt.Errorf("source store %s is not below the source root", path)
	}

	ancestors, err := sourceAncestors(path)
	if err != nil {
		return nil, false, err
	}

	var releases []func()

	release := func() {
		for _, r := range slices.Backward(releases) {
			r()
		}
	}

	for _, dir := range ancestors {
		unlock, acquired, err := tryAcquireTreeLock(dir, false)
		if err != nil || !acquired {
			release()
			return nil, false, err
		}

		releases = append(releases, unlock)
	}

	unlockTree, acquired, err := tryAcquireTreeLock(path, true)
	if err != nil || !acquired {
		release()
		return nil, false, err
	}

	releases = append(releases, unlockTree)

	unlockGC, acquired, err := TryAcquireExclusiveGCPathLock(path)
	if err != nil || !acquired {
		release()
		return nil, false, err
	}

	releases = append(releases, unlockGC)

	var once sync.Once

	return func() { once.Do(release) }, true, nil
}

func tryAcquireTreeLock(dir string, exclusive bool) (func(), bool, error) {
	key := treeLockKey(dir)
	mutex := treeLockMutex(key)

	mode, lock, unlock := unix.LOCK_SH, mutex.TryRLock, mutex.RUnlock
	if exclusive {
		mode, lock, unlock = unix.LOCK_EX, mutex.TryLock, mutex.Unlock
	}

	if !lock() {
		return nil, false, nil
	}

	unlockFile, acquired, err := tryAcquireCrossProcessLockMode(key, mode)
	if err != nil || !acquired {
		unlock()
		return nil, false, err
	}

	return func() {
		unlockFile()
		unlock()
	}, true, nil
}

// recordSourceUse marks the store whose GC gate has key as used now. Failures only make the store look older.
func recordSourceUse(key string) {
	now := time.Now()
	_ = os.Chtimes(key+".lock", now, now)
}

// LastSourceUse returns when the source store at path was last used by a holder of its GC gate (see
// AcquireSharedGCPathLock), or the zero time if it never was.
func LastSourceUse(path string) (time.Time, error) {
	info, err := os.Stat(canonicalLockKey(path+gcLockSuffix) + ".lock")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, nil
		}

		return time.Time{}, err
	}

	return info.ModTime(), nil
}

// HasNestedSourceStores reports whether a source store nested in dir has ever taken its GC gate, which leaves dir's
// tree lock file behind.
func HasNestedSourceStores(dir string) (bool, error) {
	_, err := os.Lstat(treeLockKey(dir) + ".lock")
	if err == nil {
		return true, nil
	}

	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	return false, err
}

// IsSourceGateLockFile reports whether name is the name of a GC gate or tree lock file, which only exist next to the
// base directory of a source store or a directory a source store is nested in.
func IsSourceGateLockFile(name string) bool {
	return strings.HasSuffix(name, gcLockSuffix+".lock") || strings.HasSuffix(name, treeLockSuffix+".lock")
}
