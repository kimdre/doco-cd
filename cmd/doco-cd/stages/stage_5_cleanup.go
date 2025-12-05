package stages

import (
	"context"
	"time"
)

func (s *StageManager) RunCleanupStage(_ context.Context) error {
	s.Stages.Cleanup.StartedAt = time.Now()

	defer func() {
		s.Stages.Cleanup.FinishedAt = time.Now()
	}()

	return nil
}
