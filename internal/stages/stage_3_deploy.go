package stages

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
)

func (s *StageManager) RunDeployStage(ctx context.Context, stageLog *slog.Logger) error {
	s.Stages.Deploy.StartedAt = time.Now()

	defer func() {
		s.Stages.Deploy.FinishedAt = time.Now()
	}()

	var err error

	latestCommit := strings.TrimSpace(s.Repository.Revision)
	if s.Repository.Source != config.SourceTypeOCI {
		latestCommit, err = git.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
		if err != nil {
			return fmt.Errorf("failed to get latest commit: %w", err)
		}
	}

	err = docker.DeployStack(stageLog, s.Repository.PathExternal, &ctx, s.Docker.Cmd,
		s.Payload, s.DeployConfig,
		s.DeployState.changedServices, s.DeployState.ignoredInfo.NeedSendSignal,
		latestCommit, app.Version, s.Docker.SwarmMode)
	if err != nil {
		return fmt.Errorf("failed to deploy stack %s: %w", s.DeployConfig.Name, err)
	}

	return nil
}
