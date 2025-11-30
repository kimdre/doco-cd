package main

import "sync"

const maxDeploymentLoopCount = 3 // Maximum allowed deployment loops before taking action.

// deploymentLoopTracker keeps track of deployment loops for different stacks.
var deploymentLoopTracker = struct {
	sync.Mutex
	loops map[string]struct {
		lastCommit string
		count      int
	}
}{loops: make(map[string]struct {
	lastCommit string
	count      int
})}

// shouldForceDeploy checks if a deployment loop is detected for the given stackName
// based on the latestCommit. It returns true if the deployment should be forced.
func shouldForceDeploy(stackName, latestCommit string) bool {
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
