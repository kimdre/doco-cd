package stages

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/notification"
)

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

func (s *StageManager) RunDeployStage(ctx context.Context) error {
	s.Stages.Deploy.StartedAt = time.Now()

	defer func() {
		s.Stages.Deploy.FinishedAt = time.Now()
	}()

	latestCommit, err := git.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
	if err != nil {
		return fmt.Errorf("failed to get latest commit: %w", err)
	}

	forceDeploy := shouldForceDeploy(s.DeployConfig.Name, latestCommit, s.AppConfig.MaxDeploymentLoopCount)
	if forceDeploy {
		s.Log.Warn("deployment loop detected for stack, forcing deployment", slog.String("commit", latestCommit))
	}

	// TODO: Change DeployStack args and remove the metadata construction here
	metadata := notification.Metadata{
		Repository: s.Repository.Name,
		Stack:      s.DeployConfig.Name,
		Revision:   notification.GetRevision(s.DeployConfig.Reference, latestCommit),
		JobID:      s.JobID,
	}

	fmt.Println(s.Payload)

	err = docker.DeployStack(s.Log, s.Repository.PathInternal, s.Repository.PathExternal, &ctx, &s.Docker.Cmd, s.Docker.Client,
		s.Payload, s.DeployConfig, s.DeployState.ChangedFiles, latestCommit, config.AppVersion,
		"poll", forceDeploy, metadata, s.DeployState.ResolvedSecrets, s.DeployState.SecretsChanged)
	if err != nil {
		return fmt.Errorf("failed to deploy stack %s: %w", s.DeployConfig.Name, err)
	}

	return nil
}
