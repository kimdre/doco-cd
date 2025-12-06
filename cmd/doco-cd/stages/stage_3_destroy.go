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
)

func (s *StageManager) RunDestroyStage(ctx context.Context, stageLog *slog.Logger) error {
	s.Stages.Deploy.StartedAt = time.Now()

	defer func() {
		s.Stages.Deploy.FinishedAt = time.Now()
	}()

	stageLog.Debug("destroying stack")

	// Check if doco-cd manages the stack
	managed := false

	serviceLabels, err := docker.GetServiceLabels(ctx, s.Docker.Client, s.DeployConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to retrieve service labels: %w", err)
	}

	// If no containers are found, skip the destruction step
	if len(serviceLabels) == 0 {
		stageLog.Info("no services found for the stack, skipping destruction")
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

	err = docker.DestroyStack(stageLog, &ctx, &s.Docker.Cmd, s.DeployConfig)
	if err != nil {
		return fmt.Errorf("failed to destroy stack: %w", err)
	}

	if swarm.ModeEnabled && s.DeployConfig.DestroyOpts.RemoveVolumes {
		err = docker.RemoveLabeledVolumes(ctx, s.Docker.Client, s.DeployConfig.Name)
		if err != nil {
			return fmt.Errorf("failed to remove volumes: %w", err)
		}
	}

	if s.DeployConfig.DestroyOpts.RemoveRepoDir {
		// Remove the repository directory after destroying the stack
		stageLog.Debug("removing deployment directory", slog.String("path", s.Repository.PathExternal))
		// Check if the parent directory has multiple subdirectories/repos
		parentDir := filepath.Dir(s.Repository.PathInternal)

		subDirs, err := os.ReadDir(parentDir)
		if err != nil {
			return fmt.Errorf("failed to read parent directory: %w", err)
		}

		if len(subDirs) > 1 {
			// Do not remove the parent directory if it has multiple subdirectories
			stageLog.Debug("remove deployment directory but keep parent directory as it has multiple subdirectories", slog.String("path", s.Repository.PathInternal))

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
