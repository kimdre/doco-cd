package git

import "sync"

// cloneLocks serializes clone/update operations per repository path to avoid
// race conditions where concurrent clones produce partial repo state.
var cloneLocks = struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}{
	m: make(map[string]*sync.Mutex),
}

// lockForPath returns the mutex for a given path, creating it if necessary.
func lockForPath(p string) *sync.Mutex {
	cloneLocks.mu.Lock()
	defer cloneLocks.mu.Unlock()

	if m, ok := cloneLocks.m[p]; ok {
		return m
	}

	m := &sync.Mutex{}
	cloneLocks.m[p] = m

	return m
}
