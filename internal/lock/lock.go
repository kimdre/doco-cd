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

// stackLocks holds one mutex per stack name, used to enforce mutual exclusion
// between a deployment and a scheduled job run for the same stack.
var stackLocks sync.Map

// GetRepoLock retrieves or creates a RepoLock for the given repoName.
func GetRepoLock(repoName string) *RepoLock {
	lockIface, _ := repoLocks.LoadOrStore(repoName, &RepoLock{})
	return lockIface.(*RepoLock)
}

// getStackMutex returns the mutex for the given stack name, creating it if needed.
func getStackMutex(stackName string) *sync.Mutex {
	mu, _ := stackLocks.LoadOrStore(stackName, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// LockStack acquires the per-stack scheduler/deployment lock for stackName.
// While held, scheduled runs and deployments for this specific stack are mutually
// exclusive. Different stacks do not block each other.
// If stackName is empty the call is a no-op.
func LockStack(stackName string) {
	if stackName == "" {
		return
	}

	getStackMutex(stackName).Lock()
}

// UnlockStack releases the per-stack scheduler/deployment lock for stackName.
// If stackName is empty the call is a no-op.
func UnlockStack(stackName string) {
	if stackName == "" {
		return
	}

	getStackMutex(stackName).Unlock()
}
