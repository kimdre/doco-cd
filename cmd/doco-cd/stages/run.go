package stages

import (
	"context"
	"errors"
	"log/slog"
)

// StageOrder holds the ordered list of stage names and their corresponding functions.
type StageOrder struct {
	Order []StageName                                   // The order of stages to be executed
	Funcs map[StageName]func(ctx context.Context) error // Mapping of stage names to their execution functions
}

// GetDeployStageOrder returns the order of stages for the deployment process.
func (s *StageManager) GetDeployStageOrder() StageOrder {
	return StageOrder{
		Order: []StageName{
			StageInit,
			StagePreDeploy,
			StageDeploy,
			StagePostDeploy,
			StageCleanup,
		},
		Funcs: map[StageName]func(ctx context.Context) error{
			StageInit:       func(ctx context.Context) error { return s.RunInitStage(ctx) },
			StagePreDeploy:  func(ctx context.Context) error { return s.RunPreDeployStage(ctx) },
			StageDeploy:     func(ctx context.Context) error { return s.RunDeployStage(ctx) },
			StagePostDeploy: func(ctx context.Context) error { return s.RunPostDeployStage(ctx) },
			StageCleanup:    func(ctx context.Context) error { return s.RunCleanupStage(ctx) },
		},
	}
}

// GetDestroyStageOrder returns the order of stages for the destroy process.
func (s *StageManager) GetDestroyStageOrder() StageOrder {
	return StageOrder{
		Order: []StageName{
			StageInit,
			StageDestroy,
			StageCleanup,
		},
		Funcs: map[StageName]func(ctx context.Context) error{
			StageInit:    func(ctx context.Context) error { return s.RunInitStage(ctx) },
			StageDestroy: func(ctx context.Context) error { return s.RunDestroyStage(ctx) },
			StageCleanup: func(ctx context.Context) error { return s.RunCleanupStage(ctx) },
		},
	}
}

// RunStages executes the stages in the defined order.
func (s *StageManager) RunStages(ctx context.Context) error {
	stageOrder := s.GetDeployStageOrder()
	if s.DeployConfig.Destroy {
		stageOrder = s.GetDestroyStageOrder()
	}

	for _, stageName := range stageOrder.Order {
		s.Log.Debug(string("begin stage: "+stageName), slog.String("stage", string(stageName)))

		err := stageOrder.Funcs[stageName](ctx)
		if err != nil {
			s.Log.Debug(string("end stage early: "+stageName), slog.String("stage", string(stageName)), slog.String("reason", err.Error()))
			// If the error is ErrSkipDeployment, we don't treat it as a failure
			if errors.Is(err, ErrSkipDeployment) {
				return nil
			}

			s.NotifyFailure(err)

			return err
		}

		s.Log.Debug(string("completed stage "+stageName), slog.String("stage", string(stageName)))
	}

	return nil
}
