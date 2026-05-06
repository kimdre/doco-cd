package stages

import (
	"context"
	"log/slog"
	"time"

	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
)

func (s *StageManager) RunPostDestroyStage(_ context.Context, stageLog *slog.Logger) error {
	s.Stages.PostDestroy.StartedAt = time.Now()

	defer func() {
		s.Stages.PostDestroy.FinishedAt = time.Now()
	}()

	metadata := s.Metadata
	metadata.Repository = s.Repository.Name
	metadata.Stack = s.DeployConfig.Name
	metadata.JobID = s.JobID

	err := notification.Send(notification.Success, "Stack destroyed", "successfully destroyed stack "+s.DeployConfig.Name, metadata)
	if err != nil {
		stageLog.Error("failed to send notification", logger.ErrAttr(err))
	}

	return nil
}
