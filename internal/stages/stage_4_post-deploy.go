package stages

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	gogit "github.com/go-git/go-git/v5"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
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
		err = s.withMirrorRead(func(repo *gogit.Repository) error {
			// This stage reports what this run deployed. A parallel webhook may have
			// advanced the mirror's branch already, so only resolve the moving ref for
			// legacy callers that did not record an immutable revision.
			latestCommit = strings.TrimSpace(s.Repository.Revision)
			if latestCommit == "" {
				latestCommit, err = git.GetLatestCommit(repo, s.DeployConfig.Reference)
				if err != nil {
					return fmt.Errorf("failed to get latest commit: %w", err)
				}
			}

			shortCommit, err = git.GetShortestUniqueCommitHash(repo, latestCommit, git.DefaultShortSHALength)
			if err != nil {
				return fmt.Errorf("failed to get short commit SHA: %w", err)
			}

			return nil
		})
		if err != nil {
			return err
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
		// Only commits that touch the files of this stack belong in its changelog, so a
		// repository with several stacks does not report the changes of all of them.
		// A nil filter walks the log unfiltered, which is what a project without any
		// resolvable path in the repository falls back to.
		// The deployment configuration is passed alongside the project because it is not
		// part of it: it declares the stack and holds its image tags, so a commit that
		// touches only it is precisely the commit that caused this deploy.
		// It is read inside the container, so it is relative to the internal repo path.
		var extraRepoPaths []string
		if rel, ok := docker.RepoRelativePath(s.Repository.PathInternal, s.DeployConfig.Internal.File); ok {
			extraRepoPaths = append(extraRepoPaths, rel)
		}

		pathFilter, filterErr := docker.ProjectPathFilter(
			s.Repository.PathExternal,
			s.Docker.Project,
			extraRepoPaths...,
		)
		if filterErr != nil {
			stageLog.Warn("failed to build changelog path filter, listing all commits", logger.ErrAttr(filterErr))
		}

		metadata.Commits, err = mirrorRead(s, func(repo *gogit.Repository) ([]git.CommitInfo, error) {
			return git.GetCommitsBetween(
				stageLog,
				repo,
				plumbing.NewHash(s.DeployState.DeployedCommit),
				plumbing.NewHash(latestCommit),
				maxChangelogCommits,
				pathFilter,
			)
		})
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
