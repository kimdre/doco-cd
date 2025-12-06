package stages

import (
	"context"
	"log/slog"
	"time"
)

func (s *StageManager) RunCleanupStage(_ context.Context, _ *slog.Logger) error {
	s.Stages.Cleanup.StartedAt = time.Now()

	defer func() {
		s.Stages.Cleanup.FinishedAt = time.Now()
	}()

	return nil
}
