package stages

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/migration"
)

func (s *StageManager) RunCleanupStage(ctx context.Context, log *slog.Logger) error {
	s.Stages.Cleanup.StartedAt = time.Now()

	defer func() {
		s.Stages.Cleanup.FinishedAt = time.Now()
	}()

	// The legacy checkout layout only existed for Git sources. OCI stores have never contained
	// these flat working-tree files, so avoid probing their repository directories on every run.
	if s.Contexts != nil && s.Docker != nil && s.Repository != nil && s.Repository.Source != config.SourceTypeOCI {
		repoDir := filepath.Join(s.Docker.DataMountPoint.Destination, s.Repository.Name)

		if err := migration.CleanupRepoLeftovers(
			ctx, log, s.LeftoverTracker, s.Contexts,
			s.Docker.DataMountPoint.Source, s.Docker.DataMountPoint.Destination, repoDir,
		); err != nil {
			log.Warn("failed to clean up legacy on-disk repository leftovers", logger.ErrAttr(err))
		}
	}

	return nil
}
