package stages

import (
	"context"
	"time"
)

func (s *StageManager) RunPostDeployStage(_ context.Context) error {
	s.Stages.PostDeploy.StartedAt = time.Now()

	defer func() {
		s.Stages.PostDeploy.FinishedAt = time.Now()
	}()

	return nil
}
