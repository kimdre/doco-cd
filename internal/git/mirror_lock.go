package git

import (
	"sync"
	"time"

	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

// Mirror lock modes reported to the mirror lock observer.
const (
	MirrorLockShared    = "shared"
	MirrorLockExclusive = "exclusive"
)

// MirrorLockObserver receives how long a bare mirror lock was waited for and held.
type MirrorLockObserver func(mode string, waited, held time.Duration)

var (
	mirrorLockObserverMu sync.RWMutex
	mirrorLockObserver   MirrorLockObserver = func(string, time.Duration, time.Duration) {}
)

// SetMirrorLockObserver configures an observer for bare mirror lock acquisitions.
func SetMirrorLockObserver(observer MirrorLockObserver) {
	mirrorLockObserverMu.Lock()
	defer mirrorLockObserverMu.Unlock()

	if observer == nil {
		mirrorLockObserver = func(string, time.Duration, time.Duration) {}

		return
	}

	mirrorLockObserver = observer
}

func observeMirrorLock(mode string, waited, held time.Duration) {
	mirrorLockObserverMu.RLock()

	observer := mirrorLockObserver

	mirrorLockObserverMu.RUnlock()

	observer(mode, waited, held)
}

// AcquireSharedMirrorLock takes the shared path lock of the bare mirror at
// mirrorDir and reports its wait and hold time once released.
func AcquireSharedMirrorLock(mirrorDir string) func() {
	return acquireObservedMirrorLock(MirrorLockShared, mirrorDir, sourcecache.AcquireSharedPathLock)
}

// AcquireExclusiveMirrorLock takes the exclusive path lock of the bare mirror at
// mirrorDir and reports its wait and hold time once released.
func AcquireExclusiveMirrorLock(mirrorDir string) func() {
	return acquireObservedMirrorLock(MirrorLockExclusive, mirrorDir, sourcecache.AcquireExclusivePathLock)
}

func acquireObservedMirrorLock(mode, mirrorDir string, acquire func(string) func()) func() {
	requestedAt := time.Now()
	unlock := acquire(mirrorDir)
	acquiredAt := time.Now()

	var once sync.Once

	return func() {
		once.Do(func() {
			held := time.Since(acquiredAt)

			unlock()
			observeMirrorLock(mode, acquiredAt.Sub(requestedAt), held)
		})
	}
}
