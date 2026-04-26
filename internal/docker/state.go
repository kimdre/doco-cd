package docker

import (
	"strings"
	"sync"
)

type deployStatus struct {
	ComposeHash string
	CommitSHA   string
}

// for case when some services(not all) are removed, the compose_hash, commit_sha will change,
// but docker compose will not recreate the remaining services(compose_hash, commit_sha),
// the next time when we compare the deployment status, we will get the old compose_hash and commit_sha.
//
// so we need to the cache the deployment status after successful deployment
// https://github.com/kimdre/doco-cd/issues/1262

// map[repoName:deployName]deployStatus
// repoName should use git.RepoName() function to get host/owner/repo, e.g., github.com/kimdre/doco-cd.
var deployStatusCache sync.Map

func getDeployStatusCacheKey(repoName string, deployName string) string {
	return strings.Join([]string{repoName, deployName}, ":")
}

func getDeployStatusFromCache(cacheMap *sync.Map, repoName string, deployName string) (deployStatus, bool) {
	if value, ok := cacheMap.Load(getDeployStatusCacheKey(repoName, deployName)); ok {
		if status, valid := value.(deployStatus); valid {
			return status, true
		}
	}

	return deployStatus{}, false
}

func setDeployStatusToCache(repoName string, deployName string, status deployStatus) {
	deployStatusCache.Store(getDeployStatusCacheKey(repoName, deployName), status)
}
