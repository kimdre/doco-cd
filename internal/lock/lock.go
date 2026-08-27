package lock

import (
	"context"
	"strings"
	"sync"
)

// RepoLock represents a lock for a specific repository.
type RepoLock struct {
	mu      sync.Mutex
	locked  bool
	holder  string
	waiters []*repoLockWaiter
}

type repoLockWaiter struct {
	jobID   string
	granted chan struct{}
}

// TryLock attempts to acquire the lock for the given jobID.
// It returns true if the lock was successfully acquired.
func (l *RepoLock) TryLock(jobID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.locked {
		return false
	}

	l.locked = true
	l.holder = jobID

	return true
}

// Unlock releases the lock.
func (l *RepoLock) Unlock() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.waiters) == 0 {
		l.locked = false
		l.holder = ""

		return
	}

	waiter := l.waiters[0]
	l.waiters = l.waiters[1:]
	l.holder = waiter.jobID
	close(waiter.granted)
}

// Lock acquires the lock, blocking until it is available.
func (l *RepoLock) Lock() {
	l.LockContext(context.Background(), "")
}

// LockContext acquires the lock in waiter order or returns false when ctx is cancelled.
func (l *RepoLock) LockContext(ctx context.Context, jobID string) bool {
	l.mu.Lock()
	if !l.locked {
		select {
		case <-ctx.Done():
			l.mu.Unlock()

			return false
		default:
		}

		l.locked = true
		l.holder = jobID
		l.mu.Unlock()

		return true
	}

	waiter := &repoLockWaiter{jobID: jobID, granted: make(chan struct{})}
	l.waiters = append(l.waiters, waiter)
	l.mu.Unlock()

	select {
	case <-waiter.granted:
		return true
	case <-ctx.Done():
		l.mu.Lock()
		for i, queued := range l.waiters {
			if queued == waiter {
				l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
				l.mu.Unlock()

				return false
			}
		}
		l.mu.Unlock()

		l.Unlock()

		return false
	}
}

// Holder returns the jobID of the current lock holder.
func (l *RepoLock) Holder() string {
	l.mu.Lock()
	defer l.mu.Unlock()

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

// StackKey builds the per-stack lock key for a stack deployed to a Docker context.
// Stack names are only unique within a Docker context, so same-named stacks on
// different contexts must not block each other. The default context keeps the bare
// stack name so callers that only ever operate on it (the job scheduler and the
// certificate rotation watcher) stay mutually exclusive with its deployments.
func StackKey(contextName, stackName string) string {
	contextName = strings.TrimSpace(contextName)
	if contextName == "" || strings.EqualFold(contextName, "default") {
		return stackName
	}

	return contextName + "/" + stackName
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
