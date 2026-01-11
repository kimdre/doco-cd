package main

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

// deploymentLoopTracker keeps track of deployment loops for different stacks.
var deploymentLoopTracker = struct {
	sync.Mutex
	loops map[string]struct {
		lastCommit string
		count      uint
	}
}{loops: make(map[string]struct {
	lastCommit string
	count      uint
})}

// shouldForceDeploy checks if a deployment loop is detected for the given stackName
// based on the latestCommit. It returns true if the deployment should be forced.
func shouldForceDeploy(stackName, latestCommit string, maxDeploymentLoopCount uint) bool {
	if maxDeploymentLoopCount == 0 {
		return false
	}

	deploymentLoopTracker.Lock()
	defer deploymentLoopTracker.Unlock()

	loopInfo := deploymentLoopTracker.loops[stackName]
	if loopInfo.lastCommit == latestCommit {
		loopInfo.count++
	} else {
		loopInfo.lastCommit = latestCommit
		loopInfo.count = 1
	}

	deploymentLoopTracker.loops[stackName] = loopInfo

	return loopInfo.count >= maxDeploymentLoopCount
}
