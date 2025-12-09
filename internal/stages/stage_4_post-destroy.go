package stages

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
)

func (s *StageManager) RunPostDestroyStage(_ context.Context, stageLog *slog.Logger) error {
	s.Stages.PostDeploy.StartedAt = time.Now()

	defer func() {
		s.Stages.PostDeploy.FinishedAt = time.Now()
	}()

	latestCommit, err := git.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
	if err != nil {
		return fmt.Errorf("failed to get latest commit: %w", err)
	}

	shortCommit, err := git.GetShortestUniqueCommitSHA(s.Repository.Git, latestCommit, git.DefaultShortSHALength)
	if err != nil {
		return fmt.Errorf("failed to get short commit sha: %w", err)
	}

	metadata := notification.Metadata{
		Repository: s.Repository.Name,
		Stack:      s.DeployConfig.Name,
		Revision:   notification.GetRevision(s.DeployConfig.Reference, shortCommit),
		JobID:      s.JobID,
	}

	err = notification.Send(notification.Success, "Stack destroyed", "successfully destroyed stack "+s.DeployConfig.Name, metadata)
	if err != nil {
		stageLog.Error("failed to send notification", logger.ErrAttr(err))
	}

	return nil
}
