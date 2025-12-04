package stages

import (
	"errors"
	"log/slog"
	"time"
)

var NotImplementedError = errors.New("not implemented")

// StageOrder defines the order of stages to be executed in the deployment process.
type StageOrder map[StageName]func() error

// GetDefaultStageOrder returns the default order of stages for the deployment process.
func (s *StageManager) GetDefaultStageOrder() StageOrder {
	return StageOrder{
		StageInit:       func() error { return s.RunInitStage() },
		StagePreDeploy:  func() error { return s.RunPreDeployStage() },
		StageDeploy:     func() error { return s.RunDeployStage() },
		StagePostDeploy: func() error { return s.RunPostDeployStage() },
		StageCleanup:    func() error { return s.RunCleanupStage() },
	}
}

// RunStages executes the stages in the defined order.
func (s *StageManager) RunStages() {
	stageOrder := s.GetDefaultStageOrder()

	for stageName, stageFunc := range stageOrder {
		s.Log.Debug(string("begin stage "+stageName), slog.String("stage", string(stageName)))

		err := stageFunc()
		if err != nil {
			s.Fail(stageName, err)
			return
		}
		s.Log.Debug(string("completed stage "+stageName), slog.String("stage", string(stageName)))
	}

	s.Notify("deployment completed successfully")
}

func (s *StageManager) RunPreDeployStage() error {
	s.Stages.PreDeploy.MetaData.StartedAt = time.Now()

	defer func() {
		s.Stages.PreDeploy.MetaData.FinishedAt = time.Now()
	}()

	return NotImplementedError
}

func (s *StageManager) RunDeployStage() error {
	s.Stages.Deploy.MetaData.StartedAt = time.Now()

	defer func() {
		s.Stages.Deploy.MetaData.FinishedAt = time.Now()
	}()

	return NotImplementedError
}

func (s *StageManager) RunPostDeployStage() error {
	s.Stages.PostDeploy.MetaData.StartedAt = time.Now()

	defer func() {
		s.Stages.PostDeploy.MetaData.FinishedAt = time.Now()
	}()

	return NotImplementedError
}

func (s *StageManager) RunCleanupStage() error {
	s.Stages.Cleanup.MetaData.StartedAt = time.Now()

	defer func() {
		s.Stages.Cleanup.MetaData.FinishedAt = time.Now()
	}()

	return NotImplementedError
}
