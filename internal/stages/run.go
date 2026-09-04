package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/common/lifecycle"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/prometheus"
)

type StageFunc func(ctx context.Context, stageLog *slog.Logger) error

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

func shouldPostPendingCommitStatus(stageName StageName, destroyEnabled, pendingPosted bool) bool {
	return !destroyEnabled && !pendingPosted && stageName == StagePreDeploy
}

func shouldSendDeploymentStartedNotification(stageName StageName, destroyEnabled, startedNotified bool) bool {
	return !destroyEnabled && !startedNotified && stageName == StagePreDeploy
}

func shouldPostFailureCommitStatus(destroyEnabled bool) bool {
	return !destroyEnabled
}

func shouldPostWebhookCommitStatus(jobTrigger JobTrigger, destroyEnabled bool) bool {
	return jobTrigger == JobTriggerWebhook && !destroyEnabled
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
	startedNotified := false

	var finishedAt time.Time

	for _, stageName := range stageOrder.Order {
		stageLog := s.Log.With(slog.String("stage", string(stageName)))

		metadata, err := s.GetStageMetaData(stageName)
		if err != nil {
			return err
		}

		stageLog.Debug(string("begin stage: " + stageName))

		err = stageOrder.Funcs[stageName](ctx, stageLog)

		outcome := "success"
		if err != nil {
			outcome = "failure"
			if errors.Is(err, ErrSkipDeployment) || errors.Is(err, ErrWebhookFilterMismatch) {
				outcome = "skipped"
			}
		}

		prometheus.DeploymentStageDuration.WithLabelValues(
			deploymentMetricsRepository(s.Repository),
			deploymentMetricsName(s.DeployConfig),
			deploymentMetricsContext(s.DeployConfig),
			string(stageName),
			outcome,
		).Observe(metadata.FinishedAt.Sub(metadata.StartedAt).Seconds())

		if err != nil {
			stageLog.Debug(string("end stage early: "+stageName),
				slog.String("reason", err.Error()),
				slog.String("duration", metadata.FinishedAt.Sub(metadata.StartedAt).Truncate(time.Millisecond).String()))
			// Skip outcomes propagate without failure reporting so callers can
			// distinguish an intentional no-op from a successful deployment.
			if errors.Is(err, ErrSkipDeployment) {
				return err
			}

			return s.handleStageFailure(ctx, stageName, stageLog, err)
		}

		stageLog.Debug(string("completed stage: "+stageName),
			slog.String("duration", metadata.FinishedAt.Sub(metadata.StartedAt).Truncate(time.Millisecond).String()))
		finishedAt = metadata.FinishedAt

		if shouldSendDeploymentStartedNotification(stageName, s.DeployConfig.Destroy.Enabled, startedNotified) {
			if err := s.NotifyDeploymentStarted(); err != nil {
				stageLog.Error("failed to send notification", slog.Any("error", err))
			}

			startedNotified = true
		}

		// Post pending statuses only after pre-deploy confirms work is required.
		if shouldPostPendingCommitStatus(stageName, s.DeployConfig.Destroy.Enabled, pendingPosted) {
			s.PostQueuedCommitStatus(ctx)
			s.PostCommitStatus(ctx, commitstatus.StatePending, "In Progress")

			pendingPosted = true
		}
	}

	if !s.DeployConfig.Destroy.Enabled {
		s.PostCommitStatus(ctx, commitstatus.StateSuccess, successfulCommitStatusDescription(s.Stages.Init.StartedAt, finishedAt))
	}

	// Success (deploy or destroy) closes any recorded failure, retries stop.
	s.clearDeploymentFailure()

	return nil
}

// handleStageFailure preserves retry safety for interrupted deploys without
// reporting the process's own shutdown as an operator-actionable failure.
func (s *StageManager) handleStageFailure(ctx context.Context, stageName StageName, stageLog *slog.Logger, err error) error {
	s.recordDeploymentFailure(stageName, err)

	if lifecycle.IsCancellation(err) {
		stageLog.Debug("deployment canceled during application shutdown", slog.String("reason", err.Error()))

		return err
	}

	notifiedErr := s.NotifyFailure(err)

	if shouldPostFailureCommitStatus(s.DeployConfig.Destroy.Enabled) {
		s.PostCommitStatus(ctx, failureCommitStatusState(stageName), commitstatus.FailureDescription(err))
	}

	return notifiedErr
}

func deploymentMetricsRepository(repository *RepositoryData) string {
	if repository == nil || strings.TrimSpace(repository.Name) == "" {
		return "unknown"
	}

	return strings.TrimSpace(repository.Name)
}

func deploymentMetricsName(config *deploy.Config) string {
	if config == nil || strings.TrimSpace(config.Name) == "" {
		return "unknown"
	}

	return strings.TrimSpace(config.Name)
}

func deploymentMetricsContext(config *deploy.Config) string {
	if config == nil {
		return "default"
	}

	return docker.DisplayContextName(config.Context)
}

// PostQueuedCommitStatus reports a resolved webhook deployment that requires
// work. Poll and destroy operations do not publish queued statuses.
func (s *StageManager) PostQueuedCommitStatus(ctx context.Context) {
	if shouldPostWebhookCommitStatus(s.JobTrigger, s.DeployConfig.Destroy.Enabled) {
		s.PostCommitStatus(ctx, commitstatus.StatePending, "Queued")
	}
}

// stageRecordsDeploymentFailure reports whether a failure of the stage must be
// recorded for retry. From the deploy stage on, the environment can be half
// mutated with the new commit already stamped on the containers, so without a
// record the next run sees "no changes" and never retries (#1702). Init and
// pre-deploy failures leave the old state in place and retry naturally.
func stageRecordsDeploymentFailure(stageName StageName) bool {
	switch stageName {
	case StageDeploy, StagePostDeploy, StageCleanup:
		return true
	default:
		return false
	}
}

// recordDeploymentFailure records the failure so the next run retries the
// deployment instead of skipping it as already deployed.
func (s *StageManager) recordDeploymentFailure(stageName StageName, cause error) {
	if s.DeployConfig.Destroy.Enabled || !stageRecordsDeploymentFailure(stageName) {
		return
	}

	docker.RecordDeploymentFailure(s.Repository.Name, s.DeployConfig.Name, docker.DeploymentFailure{
		Repository: s.Repository.Name,
		Stack:      s.DeployConfig.Name,
		CommitSHA:  s.Repository.Revision,
		Stage:      string(stageName),
		Error:      cause.Error(),
		FailedAt:   time.Now().UTC(),
	})
}

// lastDeploymentFailure returns the recorded failure of the stack's last
// deployment attempt, if any.
func (s *StageManager) lastDeploymentFailure() (docker.DeploymentFailure, bool) {
	return docker.GetDeploymentFailure(s.Repository.Name, s.DeployConfig.Name)
}

// clearDeploymentFailure removes the failure record of the stack, if present.
func (s *StageManager) clearDeploymentFailure() {
	docker.ClearDeploymentFailure(s.Repository.Name, s.DeployConfig.Name)
}
