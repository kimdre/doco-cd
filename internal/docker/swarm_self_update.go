package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/lifecycle"
	"github.com/kimdre/doco-cd/internal/common/types/clone"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// SwarmRecoveryInfo contains identity and fingerprints, not resolved deployment configuration.
type SwarmRecoveryInfo struct {
	Target           DeploymentTarget        `json:"target"`
	Desired          *DeploymentDesiredState `json:"desired,omitempty"`
	MetricsLabels    [3]string               `json:"metrics_labels"`
	MetricsStartedAt time.Time               `json:"metrics_started_at,omitzero"`
}

// finishSwarmSelfUpdate keeps reporting durable when shutdown interrupts an accepted or uncertain update.
func finishSwarmSelfUpdate(ctx context.Context, apiClient client.APIClient, handover *selfupdate.SwarmHandover, resultErr error) error {
	if handover == nil || errors.Is(resultErr, selfupdate.ErrHandover) {
		return resultErr
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if ctx.Err() != nil {
		if resultErr != nil && !lifecycle.IsCanceledOnly(resultErr) {
			current, err := selfupdate.LoadSwarmHandover(cleanupCtx, apiClient, handover.ServiceID)
			if err == nil && current != nil {
				err = selfupdate.ClaimSwarmHandover(cleanupCtx, apiClient, *current, SelfUpdateConfig().Identity.SwarmTaskID, resultErr)
			}

			return errors.Join(resultErr, err)
		}

		if resultErr == nil {
			return selfupdate.ErrHandover
		}

		return resultErr
	}

	removeErr := selfupdate.RemoveSwarmHandover(cleanupCtx, apiClient, *handover)
	if removeErr == nil {
		return resultErr
	}

	current, err := selfupdate.LoadSwarmHandover(cleanupCtx, apiClient, handover.ServiceID)
	if err != nil {
		return errors.Join(resultErr, removeErr, err)
	}

	if current == nil {
		return errors.Join(resultErr, removeErr)
	}

	// Keep the known result when cleanup fails, so this same task can finish reporting.
	if err := selfupdate.ClaimSwarmHandover(cleanupCtx, apiClient, *current, SelfUpdateConfig().Identity.SwarmTaskID, resultErr); err != nil {
		return errors.Join(resultErr, removeErr, err)
	}

	return errors.Join(selfupdate.ErrHandover, resultErr, removeErr)
}

// prepareSwarmSelfUpdate saves the pending status outside the service so rollback cannot erase it.
func prepareSwarmSelfUpdate(ctx context.Context, req runtimeDeployRequest, current swarm.Service, desired *swarm.ServiceSpec, waitIDs []string) (*selfupdate.SwarmHandover, error) {
	identity := SelfUpdateConfig().Identity
	if identity.SwarmServiceID == "" || identity.SwarmTaskID == "" || identity.SwarmServiceID != current.ID {
		return nil, errors.New("cannot journal Swarm self-update: service or task identity is unavailable")
	}

	taskHash, err := selfupdate.SwarmTaskSpecHash(desired.TaskTemplate)
	if err != nil {
		return nil, err
	}

	record := selfupdate.Record{
		ID:          uuid.NewString(),
		Stack:       req.request.DeployConfig.Name,
		Context:     req.request.DeployConfig.Context,
		Service:     identity.Service,
		Predecessor: selfupdate.ContainerRef{ID: identity.ContainerID, Name: identity.ContainerName, Number: identity.Number},
		Source:      selfSourceInfo(req.selfDeployInput()),
		Deploy:      selfupdate.DeployInfo{TimeoutSeconds: req.request.DeployConfig.Timeout.Seconds(), Services: waitIDs},
	}
	record.Source.RepoName = deploymentStateRepositoryKey(req.request)
	handover := &selfupdate.SwarmHandover{
		Record:            record,
		ServiceID:         current.ID,
		PredecessorTaskID: identity.SwarmTaskID,
		ServiceVersion:    current.Version.Index,
		TaskTemplateHash:  taskHash,
		DesiredImage:      desired.TaskTemplate.ContainerSpec.Image,
	}

	if req.request.SelfUpdateRecovery != nil {
		recovery := clone.New(req.request.SelfUpdateRecovery)
		recovery.MetricsLabels = [3]string{req.repositoryLabel, req.deploymentLabel, req.contextLabel}
		recovery.MetricsStartedAt = req.startedAt

		handover.Recovery, err = json.Marshal(recovery)
		if err != nil {
			return nil, fmt.Errorf("encode Swarm self-update recovery metadata: %w", err)
		}
	}

	if err := selfupdate.CreateSwarmHandover(ctx, req.request.DockerCLI.Client(), handover); err != nil {
		return nil, err
	}

	if desired.Labels == nil {
		desired.Labels = make(map[string]string)
	}

	desired.Labels[selfupdate.SwarmHandoverLabel] = record.ID

	req.selfDeployInput().stopPhaseReports()

	return handover, nil
}

// RecordSwarmHandoverOutcome restores the deployment cache and failure marker before queued work resumes.
func RecordSwarmHandoverOutcome(handover selfupdate.SwarmHandover) error {
	if handover.Record.Error == "" {
		RecordDeployStatus(handover.Record.Source.RepoName, handover.Record.Stack,
			handover.Record.Source.CommitSHA, handover.Record.Source.ProjectHash)
	}

	if len(handover.Recovery) == 0 {
		return nil
	}

	var recovery SwarmRecoveryInfo
	if err := json.Unmarshal(handover.Recovery, &recovery); err != nil {
		return fmt.Errorf("decode Swarm self-update recovery metadata: %w", err)
	}

	if handover.Record.Error == "" {
		DeploymentFailures.Clear(recovery.Target)
	} else {
		desired := DeploymentDesiredState{Revision: handover.Record.Source.CommitSHA, ProjectHash: handover.Record.Source.ProjectHash}
		if recovery.Desired != nil {
			desired = *recovery.Desired
		}

		attempt, _ := DeploymentFailures.Admit(recovery.Target, desired, deploy.RetryConfig{}, true)
		attempt.Start()
		attempt.Fail(DeploymentFailure{
			Repository: recovery.Target.Repository, Stack: recovery.Target.Stack,
			CommitSHA: handover.Record.Source.CommitSHA, Stage: "deploy", Error: handover.Record.Error,
		}, false)
	}

	return nil
}

// RecordSwarmHandoverMetrics counts only handovers whose final reporting record was removed.
func RecordSwarmHandoverMetrics(handover selfupdate.SwarmHandover) error {
	if len(handover.Recovery) == 0 {
		return nil
	}

	var recovery SwarmRecoveryInfo
	if err := json.Unmarshal(handover.Recovery, &recovery); err != nil {
		return fmt.Errorf("decode Swarm self-update metrics metadata: %w", err)
	}

	labels := recovery.MetricsLabels[:]
	if handover.Record.Error != "" {
		prometheus.DeploymentErrorsTotal.WithLabelValues(labels...).Inc()
	} else {
		prometheus.DeploymentsTotal.WithLabelValues(labels...).Inc()

		startedAt := recovery.MetricsStartedAt
		if startedAt.IsZero() {
			startedAt = handover.Record.CreatedAt
		}

		prometheus.DeploymentDuration.WithLabelValues(labels...).Observe(time.Since(startedAt).Seconds())
	}

	return nil
}

// WaitForSwarmSelfUpdate keeps later self-stack runs behind the pending confirmation.
// It runs before pre-deploy posts a status, so queued revisions cannot leave extra pending statuses.
func WaitForSwarmSelfUpdate(ctx context.Context, apiClient client.APIClient, dcName, contextName string, log *slog.Logger) error {
	identity := SelfUpdateConfig().Identity
	if !identity.Swarm || !identity.OK || identity.Project != dcName || NormalizeContextName(contextName) != "" {
		return nil
	}

	for {
		pending, err := selfupdate.LoadSwarmHandover(ctx, apiClient, identity.SwarmServiceID)
		if err != nil {
			return err
		}

		if pending == nil {
			return nil
		}

		log.Debug("waiting for Swarm self-update confirmation", slog.String("id", pending.Record.ID))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
