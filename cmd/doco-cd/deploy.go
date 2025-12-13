package main

import "sync"

var repoLocks sync.Map // Map to hold locks for each repository

// getRepoLock retrieves a mutex lock for the given repository name.
func getRepoLock(repoName string) *sync.Mutex {
	lockIface, _ := repoLocks.LoadOrStore(repoName, &sync.Mutex{})
	return lockIface.(*sync.Mutex)
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
