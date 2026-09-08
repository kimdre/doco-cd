package cache

import (
	"path/filepath"
	"sync"
)

var sourceLocks sync.Map

// AcquirePathLock serializes access to cached source contents for one repository path.
func AcquirePathLock(sourcePath string) func() {
	key, err := filepath.Abs(sourcePath)
	if err != nil {
		key = filepath.Clean(sourcePath)
	}

	value, _ := sourceLocks.LoadOrStore(key, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()

	var once sync.Once

	return func() {
		once.Do(mutex.Unlock)
	}
}
