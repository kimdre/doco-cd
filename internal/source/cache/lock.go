package cache

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"golang.org/x/sys/unix"
)

// sourceLocks holds source-level Mutex values.
// It is separate from repoLocks because their semantics differ.
var sourceLocks sync.Map

// pathLocker adapts a path-based lock to sync.Locker.
// It is not safe for concurrent use by multiple goroutines.
type pathLocker struct {
	path     string
	instance sync.Mutex
	release  func()
}

// NewPathLocker adapts the process-and-filesystem path lock to sync.Locker.
// It lets a callee lock multiple scoped phases without losing cross-process exclusion.
func NewPathLocker(path string) sync.Locker {
	return &pathLocker{path: path}
}

// Lock acquires the path lock, blocking until it is available.
func (l *pathLocker) Lock() {
	l.instance.Lock()
	l.release = AcquirePathLock(l.path)
}

// Unlock releases the path lock, panicking if it was not held.
func (l *pathLocker) Unlock() {
	if l.release == nil {
		panic("source cache: unlock of unlocked path")
	}

	l.release()
	l.release = nil
	l.instance.Unlock()
}

// AcquirePathLock takes an exclusive lock for a mutable source path, including across shared data volumes.
func AcquirePathLock(sourcePath string) func() {
	key := canonicalLockKey(sourcePath)

	value, _ := sourceLocks.LoadOrStore(key, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()

	unlockFile := acquireCrossProcessLock(key, unix.LOCK_EX)

	var once sync.Once

	return func() {
		once.Do(func() {
			unlockFile()
			mutex.Unlock()
		})
	}
}

// repoLocks holds repository-level RWMutex values. It is separate from sourceLocks because their semantics differ.
var repoLocks sync.Map

const gcLockSuffix = ".gc-use"

// AcquireSharedGCPathLock marks a source store as actively preparing or deploying without sharing a lock namespace with
// source mutation locks. Unlike ordinary source locks, it fails rather than silently dropping cross-process protection.
func AcquireSharedGCPathLock(path string) (func(), error) {
	key := canonicalLockKey(path + gcLockSuffix)

	value, _ := repoLocks.LoadOrStore(key, &sync.RWMutex{})
	mutex := value.(*sync.RWMutex)
	mutex.RLock()

	unlockFile, err := acquireRequiredCrossProcessLock(key, unix.LOCK_SH)
	if err != nil {
		mutex.RUnlock()
		return nil, err
	}

	var once sync.Once

	return func() {
		once.Do(func() {
			unlockFile()
			mutex.RUnlock()
		})
	}, nil
}

// AcquireExclusiveGCPathLock waits for exclusive access to a source store's GC gate.
func AcquireExclusiveGCPathLock(path string) (func(), error) {
	key := canonicalLockKey(path + gcLockSuffix)

	value, _ := repoLocks.LoadOrStore(key, &sync.RWMutex{})
	mutex := value.(*sync.RWMutex)
	mutex.Lock()

	unlockFile, err := acquireRequiredCrossProcessLock(key, unix.LOCK_EX)
	if err != nil {
		mutex.Unlock()
		return nil, err
	}

	var once sync.Once

	return func() {
		once.Do(func() {
			unlockFile()
			mutex.Unlock()
		})
	}, nil
}

// TryAcquireExclusiveGCPathLock prevents new deployments from crossing their in-flight-to-labeled transition while a
// source store is scanned. Failure to obtain cross-process exclusion skips the sweep.
func TryAcquireExclusiveGCPathLock(path string) (func(), bool, error) {
	return TryAcquireExclusivePathLock(path + gcLockSuffix)
}

// AcquireSharedPathLock takes a shared repository lock and excludes exclusive holders.
func AcquireSharedPathLock(path string) func() {
	key := canonicalLockKey(path)

	value, _ := repoLocks.LoadOrStore(key, &sync.RWMutex{})
	mutex := value.(*sync.RWMutex)
	mutex.RLock()

	unlockFile := acquireCrossProcessLock(key, unix.LOCK_SH)

	var once sync.Once

	return func() {
		once.Do(func() {
			unlockFile()
			mutex.RUnlock()
		})
	}
}

// AcquireExclusivePathLock takes an exclusive repository lock and excludes all other holders.
func AcquireExclusivePathLock(path string) func() {
	key := canonicalLockKey(path)

	value, _ := repoLocks.LoadOrStore(key, &sync.RWMutex{})
	mutex := value.(*sync.RWMutex)
	mutex.Lock()

	unlockFile := acquireCrossProcessLock(key, unix.LOCK_EX)

	var once sync.Once

	return func() {
		once.Do(func() {
			unlockFile()
			mutex.Unlock()
		})
	}
}

// AcquireRequiredExclusivePathLock takes an exclusive path lock and fails if cross-process
// exclusion cannot be established.
func AcquireRequiredExclusivePathLock(path string) (func(), error) {
	key := canonicalLockKey(path)

	value, _ := repoLocks.LoadOrStore(key, &sync.RWMutex{})
	mutex := value.(*sync.RWMutex)
	mutex.Lock()

	unlockFile, err := acquireRequiredCrossProcessLock(key, unix.LOCK_EX)
	if err != nil {
		mutex.Unlock()
		return nil, err
	}

	var once sync.Once

	return func() {
		once.Do(func() {
			unlockFile()
			mutex.Unlock()
		})
	}, nil
}

// TryAcquireExclusivePathLock attempts to take an exclusive path lock without waiting. It returns false when another
// process or goroutine currently holds the path.
func TryAcquireExclusivePathLock(path string) (func(), bool, error) {
	key := canonicalLockKey(path)

	value, _ := repoLocks.LoadOrStore(key, &sync.RWMutex{})

	mutex := value.(*sync.RWMutex)
	if !mutex.TryLock() {
		return nil, false, nil
	}

	unlockFile, acquired, err := tryAcquireCrossProcessLock(key)
	if err != nil {
		mutex.Unlock()
		return nil, false, err
	}

	if !acquired {
		mutex.Unlock()
		return nil, false, nil
	}

	var once sync.Once

	return func() {
		once.Do(func() {
			unlockFile()
			mutex.Unlock()
		})
	}, true, nil
}

// canonicalLockKey resolves equivalent paths by resolving symlinks in the longest existing prefix.
func canonicalLockKey(path string) string {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		absolutePath = filepath.Clean(path)
	}

	current := absolutePath

	var suffix []string

	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for _, s := range slices.Backward(suffix) {
				resolved = filepath.Join(resolved, s)
			}

			return filepath.Clean(resolved)
		}

		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(absolutePath)
		}

		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// acquireCrossProcessLock obtains a sibling-file flock. If unavailable, only in-process exclusion applies.
func acquireCrossProcessLock(path string, mode int) func() {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		slog.Warn("failed to create cross-process lock directory; falling back to in-process locking only",
			slog.String("path", path), slog.Any("error", err))

		return func() {}
	}

	lockPath := path + ".lock"

	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o640) //nolint:gosec // lock file path is derived from doco-cd's own resolved data paths
	if err != nil {
		slog.Warn("failed to open cross-process lock file; falling back to in-process locking only",
			slog.String("path", lockPath), slog.Any("error", err))

		return func() {}
	}

	if err = unix.Flock(int(file.Fd()), mode); err != nil {
		slog.Warn("failed to acquire cross-process lock; falling back to in-process locking only",
			slog.String("path", lockPath), slog.Any("error", err))

		_ = file.Close()

		return func() {}
	}

	return func() {
		if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
			slog.Warn("failed to release cross-process lock",
				slog.String("path", lockPath), slog.Any("error", err))
		}

		_ = file.Close()
	}
}

func acquireRequiredCrossProcessLock(path string, mode int) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}

	lockPath := path + ".lock"

	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o640) //nolint:gosec // lock file path is derived from doco-cd's own resolved data paths
	if err != nil {
		return nil, err
	}

	if err = unix.Flock(int(file.Fd()), mode); err != nil {
		_ = file.Close()
		return nil, err
	}

	return func() {
		if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
			slog.Warn("failed to release cross-process lock",
				slog.String("path", lockPath), slog.Any("error", err))
		}

		_ = file.Close()
	}, nil
}

func tryAcquireCrossProcessLock(path string) (func(), bool, error) {
	unlock, err := acquireRequiredCrossProcessLock(path, unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, nil
		}

		return nil, false, err
	}

	return unlock, true, nil
}
