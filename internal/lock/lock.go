package lock

import "sync"

// RepoLock represents a lock for a specific repository.
type RepoLock struct {
	mu     sync.Mutex
	holder string
}

// TryLock attempts to acquire the lock for the given jobID.
// It returns true if the lock was successfully acquired.
func (l *RepoLock) TryLock(jobID string) bool {
	if l.mu.TryLock() {
		l.holder = jobID
		return true
	}

	return false
}

// Unlock releases the lock.
func (l *RepoLock) Unlock() {
	l.holder = ""
	l.mu.Unlock()
}

// Lock acquires the lock, blocking until it is available.
func (l *RepoLock) Lock() {
	l.mu.Lock()
}

// Holder returns the jobID of the current lock holder.
func (l *RepoLock) Holder() string {
	return l.holder
}

var repoLocks sync.Map // Map to hold locks for each repository

// GetRepoLock retrieves or creates a RepoLock for the given repoName.
func GetRepoLock(repoName string) *RepoLock {
	lockIface, _ := repoLocks.LoadOrStore(repoName, &RepoLock{})
	return lockIface.(*RepoLock)
}
