package git

import (
	"path/filepath"
	"slices"
	"sync"
)

// cloneLocks serializes clone/update operations per repository path to avoid
// race conditions where concurrent clones produce partial repo state.
var cloneLocks = struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}{
	m: make(map[string]*sync.Mutex),
}

// lockForKey returns the mutex for a given key, creating it if necessary.
func lockForKey(k string) *sync.Mutex {
	k = canonicalLockKey(k)

	cloneLocks.mu.Lock()
	defer cloneLocks.mu.Unlock()

	if m, ok := cloneLocks.m[k]; ok {
		return m
	}

	m := &sync.Mutex{}
	cloneLocks.m[k] = m

	return m
}

// canonicalLockKey resolves equivalent paths to a shared lock key.
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

// AcquirePathLock locks the mutex for the given key and returns a function to unlock it.
// The returned unlock function is idempotent (safe to call multiple times).
func AcquirePathLock(key string) func() {
	lock := lockForKey(key)
	lock.Lock()

	var once sync.Once

	return func() { once.Do(func() { lock.Unlock() }) }
}
