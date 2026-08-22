package docker

import (
	"strings"
	"sync"
	"time"
)

/*
Failed deployments must be retried and re-reported until they succeed
(discussion #1702). Container labels cannot hold the outcome: compose stamps
them when it creates the containers, before hooks and later stages run, and
docker cannot change labels on an existing container. So the outcome lives in
daemon memory. A restart drops it and the pre-deploy comparison falls back to
the labels again - accepted trade-off, see the review of #1711.
*/

const (
	// ChangeTypeFailedDeployRetry marks a deployment retry after a recorded
	// failure. A Change of this type carries no service scope: the whole
	// project is force-recreated, because the failed part (e.g. a lifecycle
	// hook) is not attributable to one service.
	ChangeTypeFailedDeployRetry = "failed_deploy_retry"

	// maxRecordedErrorLength caps the stored error text, the full error is in the logs.
	maxRecordedErrorLength = 2000
)

// DeploymentFailure records a failed deployment attempt of one stack.
type DeploymentFailure struct {
	Repository string
	Stack      string
	CommitSHA  string
	Stage      string
	Error      string
	FailedAt   time.Time
}

// map[repoName:deployName]DeploymentFailure.
var failedDeployments sync.Map

func failedDeploymentKey(repoName, stack string) string {
	return strings.Join([]string{repoName, stack}, ":")
}

// RecordDeploymentFailure stores the failure record for a stack, replacing
// any previous one.
func RecordDeploymentFailure(repoName, stack string, failure DeploymentFailure) {
	if r := []rune(failure.Error); len(r) > maxRecordedErrorLength {
		failure.Error = string(r[:maxRecordedErrorLength])
	}

	failedDeployments.Store(failedDeploymentKey(repoName, stack), failure)
}

// GetDeploymentFailure reports whether the last deployment attempt of the
// stack failed.
func GetDeploymentFailure(repoName, stack string) (DeploymentFailure, bool) {
	if value, ok := failedDeployments.Load(failedDeploymentKey(repoName, stack)); ok {
		if failure, valid := value.(DeploymentFailure); valid {
			return failure, true
		}
	}

	return DeploymentFailure{}, false
}

// ClearDeploymentFailure removes the failure record of a stack, if present.
func ClearDeploymentFailure(repoName, stack string) {
	failedDeployments.Delete(failedDeploymentKey(repoName, stack))
}
