package stages

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/filesystem"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
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
		return ErrSkipDeployment
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
		// PathInternal names a single published artifact ("<repoDir>/artifacts/<revision>"),
		// so removing it (or its parent, the artifacts directory) would strand the mirror and
		// every sibling revision.
		// destroy.remove_repo_dir means the repository's own directory, so resolve that from the source layout instead.
		repoDir, err := filesystem.VerifyAndSanitizePath(
			filepath.Join(s.Docker.DataMountPoint.Destination, s.Repository.Name),
			s.Docker.DataMountPoint.Destination,
		)
		if err != nil {
			return fmt.Errorf("failed to resolve repository directory: %w", err)
		}

		// Exclude every concurrent Prepare call for this repository (which holds a shared lock on the same path)
		// before removing it, so this can never race a deployment that is still resolving/publishing a revision into repoDir.
		unlockRepo := sourcecache.AcquireExclusivePathLock(repoDir)
		defer unlockRepo()

		stageLog.Debug("removing repository directory", slog.String("path", s.Repository.PathExternal))

		if err = os.RemoveAll(repoDir); err != nil {
			return fmt.Errorf("failed to remove repository directory: %w", err)
		}

		// Repository names are hierarchical ("<host>/<owner>/<repo>"), so
		// clean up the ancestors this repository was the last occupant of.
		// Anything still in use - another repository, or the source lock file,
		// which must outlive the directory it guards - makes the removal fail and stops the walk, which is the intent.
		for dir := filepath.Dir(repoDir); dir != s.Docker.DataMountPoint.Destination; dir = filepath.Dir(dir) {
			if err = os.Remove(dir); err != nil {
				break
			}
		}
	}

	return nil
}
