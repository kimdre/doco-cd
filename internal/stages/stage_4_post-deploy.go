package stages

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
)

const maxChangelogCommits = 50

func (s *StageManager) RunPostDeployStage(_ context.Context, stageLog *slog.Logger) error {
	s.Stages.PostDeploy.StartedAt = time.Now()

	defer func() {
		s.Stages.PostDeploy.FinishedAt = time.Now()
	}()

	var err error

	shortCommit := strings.TrimSpace(s.Repository.Revision)

	var latestCommit string

	if s.Repository.Source != config.SourceTypeOCI {
		latestCommit, err = git.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
		if err != nil {
			return fmt.Errorf("failed to get latest commit: %w", err)
		}

		shortCommit, err = git.GetShortestUniqueCommitHash(s.Repository.Git, latestCommit, git.DefaultShortSHALength)
		if err != nil {
			return fmt.Errorf("failed to get short commit SHA: %w", err)
		}
	}

	metadata := s.Metadata
	metadata.Repository = s.Repository.Name
	metadata.Stack = s.DeployConfig.Name
	metadata.Context = s.DeployConfig.Context
	metadata.Target = s.DeployConfig.Internal.ConfigTarget
	metadata.Revision = notification.GetRevision(s.DeployConfig.Reference, shortCommit)
	metadata.JobID = s.JobID
	metadata.Duration = time.Since(s.Stages.Init.StartedAt).Truncate(time.Millisecond)
	metadata.ChangedServices = s.DeployState.changedServiceNames()

	if s.DeployState.DeployedCommit != "" && latestCommit != "" {
		metadata.Commits, err = git.GetCommitsBetween(
			s.Repository.Git,
			plumbing.NewHash(s.DeployState.DeployedCommit),
			plumbing.NewHash(latestCommit),
			maxChangelogCommits,
		)
		if err != nil {
			// changelog is best-effort, never block the notification
			stageLog.Warn("failed to build commit changelog", logger.ErrAttr(err))
		}
	}

	err = s.Notifier.Send(notification.Success, "Deployment completed", "Successfully deployed stack "+s.DeployConfig.Name, metadata)
	if err != nil {
		stageLog.Error("failed to send notification", logger.ErrAttr(err))
	}

	return nil
}
