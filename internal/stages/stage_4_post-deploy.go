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

	shortCommit := strings.TrimSpace(s.Repository.Revision)

	metadata := s.notificationMetadata()
	metadata.Duration = time.Since(s.Stages.Init.StartedAt).Truncate(time.Millisecond)

	// The success notification is sent even below the notify level, because it also
	// clears the stack's reported failure. Only the repository reads that decorate
	// it are skipped when it is not delivered.
	if s.Repository.Source != config.SourceTypeOCI && notification.WouldSend(s.Notifier, notification.Success) {
		commitSha, commits, err := s.deployedCommitDetails(stageLog)
		if err != nil {
			// The stack is already deployed and these details only decorate its
			// notification, so a failed read must not turn it into a failed deployment.
			stageLog.Warn("failed to read deployed commit details, notifying with the full revision", logger.ErrAttr(err))
		} else {
			shortCommit = commitSha
			metadata.Commits = commits
		}
	}

	metadata.Revision = notification.GetRevision(s.DeployConfig.Reference, shortCommit)

	notifyStartedAt := time.Now()

	err := s.Notifier.Send(notification.Success, "Deployment completed", "Successfully deployed stack "+s.DeployConfig.Name, metadata)
	if err != nil {
		stageLog.Error("failed to send notification", logger.ErrAttr(err))
	}

	logPostDeployOperation(stageLog, "notification", notifyStartedAt)

	return nil
}

// deployedCommitDetails returns the short SHA of the deployed commit and the
// changelog since the previously deployed commit, read in a single mirror read.
func (s *StageManager) deployedCommitDetails(stageLog *slog.Logger) (string, []git.CommitInfo, error) {
	var pathFilter func(string) bool

	if s.DeployState.DeployedCommit != "" {
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

		var filterErr error

		pathFilter, filterErr = docker.ProjectPathFilter(
			s.Repository.PathExternal,
			s.Docker.Project,
			extraRepoPaths...,
		)
		if filterErr != nil {
			stageLog.Warn("failed to build changelog path filter, listing all commits", logger.ErrAttr(filterErr))
		}
	}

	var (
		shortCommit string
		commits     []git.CommitInfo
	)

	err := s.withMirrorRead(func(repo *gogit.Repository) error {
		// This stage reports what this run deployed. A parallel webhook may have
		// advanced the mirror's branch already, so only resolve the moving ref for
		// legacy callers that did not record an immutable revision.
		var err error

		latestCommit := strings.TrimSpace(s.Repository.Revision)
		if latestCommit == "" {
			latestCommit, err = git.GetLatestCommit(repo, s.DeployConfig.Reference)
			if err != nil {
				return fmt.Errorf("failed to get latest commit: %w", err)
			}
		}

		shortSHAStartedAt := time.Now()

		shortCommit = s.shortCommitSHA(repo, latestCommit)

		logPostDeployOperation(stageLog, "short_commit_sha", shortSHAStartedAt)

		if s.DeployState.DeployedCommit == "" || latestCommit == "" {
			return nil
		}

		changelogStartedAt := time.Now()

		commits, err = git.GetCommitsBetween(
			stageLog,
			repo,
			plumbing.NewHash(s.DeployState.DeployedCommit),
			plumbing.NewHash(latestCommit),
			maxChangelogCommits,
			pathFilter,
		)
		if err != nil {
			// changelog is best-effort, never block the notification
			stageLog.Warn("failed to build commit changelog", logger.ErrAttr(err))
		}

		logPostDeployOperation(stageLog, "changelog", changelogStartedAt)

		return nil
	})

	return shortCommit, commits, err
}

func logPostDeployOperation(stageLog *slog.Logger, operation string, startedAt time.Time) {
	stageLog.Debug("completed post-deploy operation",
		slog.String("operation", operation),
		slog.String("elapsed_time", time.Since(startedAt).Truncate(time.Millisecond).String()),
	)
}
