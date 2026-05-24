package deploy

import "sync"

var (
	autoDiscoveryCacheObserverMu sync.RWMutex
	autoDiscoveryCacheObserver   = func(string, string) {}
)

// SetAutoDiscoveryCacheObserver configures an observer for auto-discovery cache lookups.
// The callback receives repository and result labels (for example: hit/miss).
func SetAutoDiscoveryCacheObserver(observer func(repository, result string)) {
	autoDiscoveryCacheObserverMu.Lock()
	defer autoDiscoveryCacheObserverMu.Unlock()

	if observer == nil {
		autoDiscoveryCacheObserver = func(string, string) {}
		return
	}

	autoDiscoveryCacheObserver = observer
}

func recordAutoDiscoveryCacheLookup(repository, result string) {
	autoDiscoveryCacheObserverMu.RLock()

	observer := autoDiscoveryCacheObserver

	autoDiscoveryCacheObserverMu.RUnlock()

	observer(repository, result)
}
