package stages

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker"
)

func (s *StageManager) RunDestroyStage(ctx context.Context, stageLog *slog.Logger) error {
	s.Stages.Destroy.StartedAt = time.Now()

	defer func() {
		s.Stages.Destroy.FinishedAt = time.Now()
	}()

	stageLog.Debug("destroying stack")

	// Check if doco-cd manages the stack
	managed := false

	serviceLabels, err := docker.GetServiceLabels(ctx, s.Docker.Cmd.Client(), s.Docker.SwarmMode, s.DeployConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to retrieve service labels: %w", err)
	}

	// If no containers are found, skip the destruction step
	if len(serviceLabels) == 0 {
		stageLog.Info("no services found for the stack, skipping destruction")
		return NothingToDestroy()
	}

	// Find deployed commit and external secrets hash from labels of deployed services
	for _, labels := range serviceLabels {
		if labels[docker.DocoCDLabels.Metadata.Manager] == app.Name {
			managed = true
			break
		}
	}

	if !managed {
		return fmt.Errorf("%w: %s: aborting destruction", ErrNotManagedByDocoCD, s.DeployConfig.Name)
	}

	err = docker.DestroyStack(stageLog, &ctx, &s.Docker.Cmd, s.DeployConfig, s.Docker.SwarmMode)
	if err != nil {
		return fmt.Errorf("failed to destroy stack: %w", err)
	}

	if s.Docker.SwarmMode && s.DeployConfig.Destroy.RemoveVolumes {
		err = docker.RemoveLabeledVolumes(ctx, s.Docker.Cmd.Client(), s.Docker.SwarmMode, s.DeployConfig.Name)
		if err != nil {
			return fmt.Errorf("failed to remove volumes: %w", err)
		}
	}

	s.requestRepositoryRemoval(stageLog)

	return nil
}

// requestRepositoryRemoval records that destroy.remove_dir asks to remove the repository directory.
// It does nothing if destroy.remove_dir is false.
//
// The directory is not removed here. It holds the artifacts and live files of every stack that
// deploys from the repository, and other stacks can still mount files from them. The caller of the
// job removes the directory after the job has released its locks, and only if no deployment
// on any Docker context still uses the repository (see internal/gc.RepositoryRemover).
func (s *StageManager) requestRepositoryRemoval(stageLog *slog.Logger) {
	if !s.DeployConfig.Destroy.RemoveRepoDir || s.Repository == nil || s.Repository.Name == "" {
		return
	}

	repoLog := stageLog.With(slog.String("repository", s.Repository.Name))

	if s.RepositoryRemovals == nil {
		repoLog.Info("skipping repository directory removal, this job cannot check other deployments of the repository; the artifact garbage collector removes unused revisions")
		return
	}

	s.RepositoryRemovals.Add(s.Repository.Name)

	repoLog.Debug("requested repository directory removal, it is removed after the job if no deployment uses the repository")
}
