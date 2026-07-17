package encryption

import (
	"sync"
	"sync/atomic"
)

type fileLock struct {
	mu   sync.Mutex
	refs atomic.Int32
}

var fileLocks sync.Map // map[string]*fileLock

func acquireFileLock(path string) *fileLock {
	lockIface, _ := fileLocks.LoadOrStore(path, &fileLock{})
	lock := lockIface.(*fileLock)
	// Atomically increment refs
	lock.refs.Add(1)
	lock.mu.Lock()

	return lock
}

func releaseFileLock(path string, lock *fileLock) {
	lock.mu.Unlock()
	// Atomically decrement refs
	if lock.refs.Add(-1) == 0 {
		fileLocks.Delete(path)
	}
}
