package stages

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
)

func (s *StageManager) RunDestroyStage(ctx context.Context) error {
	s.Stages.Deploy.StartedAt = time.Now()

	defer func() {
		s.Stages.Deploy.FinishedAt = time.Now()
	}()

	s.Log.Debug("destroying stack")

	// Check if doco-cd manages the stack
	managed := false

	serviceLabels, err := docker.GetServiceLabels(ctx, s.Docker.Client, s.DeployConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to retrieve service labels: %w", err)
	}

	// If no containers are found, skip the destruction step
	if len(serviceLabels) == 0 {
		s.Log.Info("no services found for the stack, skipping destruction")
		return ErrSkipDeployment
	}

	// Find deployed commit and external secrets hash from labels of deployed services
	for _, labels := range serviceLabels {
		if labels[docker.DocoCDLabels.Metadata.Manager] == config.AppName {
			managed = true
			break
		}
	}

	if !managed {
		return fmt.Errorf("%w: %s: aborting destruction", ErrNotManagedByDocoCD, s.DeployConfig.Name)
	}

	latestCommit, err := git.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
	if err != nil {
		return fmt.Errorf("failed to get latest commit: %w", err)
	}

	err = docker.DestroyStack(s.Log, &ctx, &s.Docker.Cmd, s.DeployConfig)
	if err != nil {
		return fmt.Errorf("failed to destroy stack: %w", err)
	}

	metadata := notification.Metadata{
		Repository: s.Repository.Name,
		Stack:      s.DeployConfig.Name,
		Revision:   notification.GetRevision(s.DeployConfig.Reference, latestCommit),
		JobID:      s.JobID,
	}

	err = notification.Send(notification.Success, "Stack destroyed", "successfully destroyed stack "+s.DeployConfig.Name, metadata)
	if err != nil {
		s.Log.Error("failed to send notification", logger.ErrAttr(err))
	}

	if swarm.ModeEnabled && s.DeployConfig.DestroyOpts.RemoveVolumes {
		err = docker.RemoveLabeledVolumes(ctx, s.Docker.Client, s.DeployConfig.Name)
		if err != nil {
			return fmt.Errorf("failed to remove volumes: %w", err)
		}
	}

	if s.DeployConfig.DestroyOpts.RemoveRepoDir {
		// Remove the repository directory after destroying the stack
		s.Log.Debug("removing deployment directory", slog.String("path", s.Repository.PathExternal))
		// Check if the parent directory has multiple subdirectories/repos
		parentDir := filepath.Dir(s.Repository.PathInternal)

		subDirs, err := os.ReadDir(parentDir)
		if err != nil {
			return fmt.Errorf("failed to read parent directory: %w", err)
		}

		if len(subDirs) > 1 {
			// Do not remove the parent directory if it has multiple subdirectories
			s.Log.Debug("remove deployment directory but keep parent directory as it has multiple subdirectories", slog.String("path", s.Repository.PathInternal))

			// Remove only the repository directory
			err = os.RemoveAll(s.Repository.PathInternal)
			if err != nil {
				return fmt.Errorf("failed to remove repository directory: %w", err)
			}
		} else {
			// Remove the parent directory if it has only one subdirectory
			err = os.RemoveAll(parentDir)
			if err != nil {
				return fmt.Errorf("failed to remove deployment directory: %w", err)
			}
		}
	}

	return nil
}
