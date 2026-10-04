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

	if s.DeployConfig.Destroy.RemoveRepoDir {
		// The source directory is a cache shared by every stack deployed from this repository: it holds the mirror,
		// every published artifact (the bind-mount sources of running stacks), their mutable live copies and any
		// legacy checkout still in use. Removing it for one stack would break all others, so the option is ignored;
		// the artifact garbage collector removes whole sources once nothing has used them for ARTIFACT_GC_SOURCE_TTL.
		stageLog.Warn("destroy.remove_dir is deprecated and ignored: the source directory is shared by all stacks of this repository "+
			"and is removed by the artifact garbage collector once it is no longer used (see ARTIFACT_GC_SOURCE_TTL)",
			slog.String("repository", s.Repository.Name))
	}

	return nil
}
