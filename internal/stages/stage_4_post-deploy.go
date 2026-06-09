package stages

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
)

func (s *StageManager) RunPostDeployStage(ctx context.Context, stageLog *slog.Logger) error {
	s.Stages.PostDeploy.StartedAt = time.Now()

	defer func() {
		s.Stages.PostDeploy.FinishedAt = time.Now()
	}()

	var err error

	shortCommit := strings.TrimSpace(s.Repository.Revision)
	if s.Repository.Source != config.SourceTypeOCI {
		latestCommit, err := git.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
		if err != nil {
			return fmt.Errorf("failed to get latest commit: %w", err)
		}

		shortCommit, err = git.GetShortestUniqueCommitHash(s.Repository.Git, latestCommit, git.DefaultShortSHALength)
		if err != nil {
			return fmt.Errorf("failed to get short commit SHA: %w", err)
		}
	}

	revision := notification.GetRevision(s.DeployConfig.Reference, shortCommit)

	metadata := s.Metadata
	metadata.Repository = s.Repository.Name
	metadata.Stack = s.DeployConfig.Name
	metadata.Revision = revision
	metadata.JobID = s.JobID

	err = notification.Send(notification.Success, "Deployment completed", "Successfully deployed stack "+s.DeployConfig.Name, metadata)
	if err != nil {
		stageLog.Error("failed to send notification", logger.ErrAttr(err))
	}

	s.fireHooks(ctx, s.DeployConfig.Hooks.OnSuccess, "success", revision, "")

	return nil
}
