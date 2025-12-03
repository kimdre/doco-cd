package stages

import "time"

// RunInitStage executes the initialization stage logic for the deployment process.
func (s *StageManager) RunInitStage() error {
	s.Stages.Init.MetaData.StartedAt = time.Now()

	defer func() {
		s.Stages.Init.MetaData.FinishedAt = time.Now()
	}()

	return NotImplementedError
}
