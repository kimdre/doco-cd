package cache

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

var sourceLocks sync.Map

// AcquirePathLock serializes access to cached source contents for one
// repository path, both within this process and across separate doco-cd
// processes/containers that share the same path (e.g. two instances mounting
// the same data volume for a self-update setup). The in-process mutex is
// acquired first to avoid redundant flock syscalls between goroutines; the
// flock then extends that exclusion to other processes.
func AcquirePathLock(sourcePath string) func() {
	key, err := filepath.Abs(sourcePath)
	if err != nil {
		key = filepath.Clean(sourcePath)
	}

	value, _ := sourceLocks.LoadOrStore(key, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()

	unlockFile := acquireCrossProcessLock(key)

	var once sync.Once

	return func() {
		once.Do(func() {
			unlockFile()
			mutex.Unlock()
		})
	}
}

// acquireCrossProcessLock blocks until it holds an exclusive flock on a lock
// file sibling to path, then returns a function that releases it. flock is
// held per open file descriptor and released automatically if the holding
// process dies, so no stale-lock cleanup is needed. If the lock file can't be
// created or locked (e.g. read-only or non-POSIX filesystem), this degrades
// to a no-op and only in-process exclusion applies, matching prior behavior.
func acquireCrossProcessLock(path string) func() {
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

	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
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
