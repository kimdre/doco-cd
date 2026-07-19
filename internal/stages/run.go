package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/commitstatus"
)

type StageFunc func(ctx context.Context, stageLog *slog.Logger) error

const maxCommitStatusDescriptionLength = 140

func successfulCommitStatusDescription(startedAt, finishedAt time.Time) string {
	if startedAt.IsZero() || finishedAt.IsZero() || finishedAt.Before(startedAt) {
		return "Successful"
	}

	duration := finishedAt.Sub(startedAt)
	if duration < time.Second {
		return "Successful in <1s"
	}

	return fmt.Sprintf("Successful in %s", duration.Round(time.Second))
}

func failureCommitStatusDescription(err error) string {
	if err == nil {
		return "Failed"
	}

	description := strings.Join(strings.Fields(err.Error()), " ")
	if len([]rune(description)) <= maxCommitStatusDescriptionLength {
		return description
	}

	truncated := []rune(description)

	return string(truncated[:maxCommitStatusDescriptionLength-3]) + "..."
}

func shouldPostPendingCommitStatus(stageName StageName, destroyEnabled, pendingPosted bool) bool {
	return !destroyEnabled && !pendingPosted && stageName == StagePreDeploy
}

func shouldPostFailureCommitStatus(destroyEnabled bool) bool {
	return !destroyEnabled
}

func failureCommitStatusState(stageName StageName) commitstatus.State {
	switch stageName {
	case StageInit, StagePreDeploy:
		return commitstatus.StateError
	default:
		return commitstatus.StateFailure
	}
}

// StageOrder holds the ordered list of stage names and their corresponding functions.
type StageOrder struct {
	Order []StageName             // The order of stages to be executed
	Funcs map[StageName]StageFunc // Mapping of stage names to their execution functions
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
		Funcs: map[StageName]StageFunc{
			StageInit:       func(ctx context.Context, stageLog *slog.Logger) error { return s.RunInitStage(ctx, stageLog) },
			StagePreDeploy:  func(ctx context.Context, stageLog *slog.Logger) error { return s.RunPreDeployStage(ctx, stageLog) },
			StageDeploy:     func(ctx context.Context, stageLog *slog.Logger) error { return s.RunDeployStage(ctx, stageLog) },
			StagePostDeploy: func(ctx context.Context, stageLog *slog.Logger) error { return s.RunPostDeployStage(ctx, stageLog) },
			StageCleanup:    func(ctx context.Context, stageLog *slog.Logger) error { return s.RunCleanupStage(ctx, stageLog) },
		},
	}
}

// GetDestroyStageOrder returns the order of stages for the destroy process.
func (s *StageManager) GetDestroyStageOrder() StageOrder {
	return StageOrder{
		Order: []StageName{
			StageInit,
			StageDestroy,
			StagePostDestroy,
			StageCleanup,
		},
		Funcs: map[StageName]StageFunc{
			StageInit:        func(ctx context.Context, stageLog *slog.Logger) error { return s.RunInitStage(ctx, stageLog) },
			StageDestroy:     func(ctx context.Context, stageLog *slog.Logger) error { return s.RunDestroyStage(ctx, stageLog) },
			StagePostDestroy: func(ctx context.Context, stageLog *slog.Logger) error { return s.RunPostDestroyStage(ctx, stageLog) },
			StageCleanup:     func(ctx context.Context, stageLog *slog.Logger) error { return s.RunCleanupStage(ctx, stageLog) },
		},
	}
}

// RunStages executes the stages in the defined order.
func (s *StageManager) RunStages(ctx context.Context) error {
	stageOrder := s.GetDeployStageOrder()
	if s.DeployConfig.Destroy.Enabled {
		stageOrder = s.GetDestroyStageOrder()
	}

	pendingPosted := false

	var finishedAt time.Time

	for _, stageName := range stageOrder.Order {
		stageLog := s.Log.With(slog.String("stage", string(stageName)))

		metadata, err := s.GetStageMetaData(stageName)
		if err != nil {
			return err
		}

		stageLog.Debug(string("begin stage: " + stageName))

		err = stageOrder.Funcs[stageName](ctx, stageLog)
		if err != nil {
			stageLog.Debug(string("end stage early: "+stageName),
				slog.String("reason", err.Error()),
				slog.String("duration", metadata.FinishedAt.Sub(metadata.StartedAt).Truncate(time.Millisecond).String()))
			// If the error is ErrSkipDeployment, we don't treat it as a failure
			if errors.Is(err, ErrSkipDeployment) {
				return nil
			}

			s.NotifyFailure(err)

			if shouldPostFailureCommitStatus(s.DeployConfig.Destroy.Enabled) {
				s.PostCommitStatus(ctx, failureCommitStatusState(stageName), failureCommitStatusDescription(err))
			}

			return err
		}

		stageLog.Debug(string("completed stage: "+stageName),
			slog.String("duration", metadata.FinishedAt.Sub(metadata.StartedAt).Truncate(time.Millisecond).String()))
		finishedAt = metadata.FinishedAt

		// Post "pending" once the repository/commit has been resolved.
		if shouldPostPendingCommitStatus(stageName, s.DeployConfig.Destroy.Enabled, pendingPosted) {
			s.PostCommitStatus(ctx, commitstatus.StatePending, "In Progress")

			pendingPosted = true
		}
	}

	if !s.DeployConfig.Destroy.Enabled {
		s.PostCommitStatus(ctx, commitstatus.StateSuccess, successfulCommitStatusDescription(s.Stages.Init.StartedAt, finishedAt))
	}

	return nil
}
