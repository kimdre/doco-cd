package cache

import (
	"context"
	"slices"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// AcquirePathLockContext takes an exclusive source-path lock with cancelable acquisition.
// It fails if cross-process exclusion cannot be established.
func AcquirePathLockContext(ctx context.Context, path string) (func(), error) {
	key := canonicalLockKey(path)
	value, _ := sourceLocks.LoadOrStore(key, &sync.Mutex{})
	mutex := value.(*sync.Mutex)

	return waitForSourceLock(ctx, func() (func(), bool, error) {
		if !mutex.TryLock() {
			return nil, false, nil
		}

		unlockFile, acquired, err := tryAcquireCrossProcessLock(key)
		if err != nil || !acquired {
			mutex.Unlock()
			return nil, false, err
		}

		return func() {
			unlockFile()
			mutex.Unlock()
		}, true, nil
	})
}

// AcquireSharedGCPathLockContext is AcquireSharedGCPathLock with cancelable acquisition.
func AcquireSharedGCPathLockContext(ctx context.Context, path string) (func(), error) {
	ancestors, err := sourceAncestors(path)
	if err != nil {
		return nil, err
	}

	var releases []func()

	release := func() {
		for _, unlock := range slices.Backward(releases) {
			unlock()
		}
	}

	for _, dir := range ancestors {
		unlock, err := waitForSourceLock(ctx, func() (func(), bool, error) {
			return tryAcquireTreeLock(dir, false)
		})
		if err != nil {
			release()
			return nil, err
		}

		releases = append(releases, unlock)
	}

	key := canonicalLockKey(path + gcLockSuffix)
	value, _ := repoLocks.LoadOrStore(key, &sync.RWMutex{})
	mutex := value.(*sync.RWMutex)

	unlock, err := waitForSourceLock(ctx, func() (func(), bool, error) {
		if !mutex.TryRLock() {
			return nil, false, nil
		}

		unlockFile, acquired, err := tryAcquireCrossProcessLockMode(key, unix.LOCK_SH)
		if err != nil || !acquired {
			mutex.RUnlock()
			return nil, false, err
		}

		recordSourceUse(key)

		return func() {
			recordSourceUse(key)
			unlockFile()
			mutex.RUnlock()
		}, true, nil
	})
	if err != nil {
		release()
		return nil, err
	}

	releases = append(releases, unlock)

	var once sync.Once

	return func() { once.Do(release) }, nil
}

// waitForSourceLock repeatedly attempts to acquire a lock using the provided tryLock function until it succeeds or the context is canceled.
func waitForSourceLock(ctx context.Context, tryLock func() (func(), bool, error)) (func(), error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		unlock, acquired, err := tryLock()
		if err != nil {
			return nil, err
		}

		if acquired {
			var once sync.Once

			return func() { once.Do(unlock) }, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
