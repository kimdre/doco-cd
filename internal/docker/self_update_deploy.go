package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// defaultSelfHealthTimeout is used when the deploy config sets no timeout.
const defaultSelfHealthTimeout = 90 * time.Second

// SelfApplierLabel marks a throwaway applier container with its journal ID.
const SelfApplierLabel = "cd.doco.self.applier"

// SelfStackLabel names the stack an applier container is updating.
const SelfStackLabel = "cd.doco.self.stack"

// prepareSelfUpdate splits a project that contains this instance into the part
// that deploys normally and a closure that performs the handover afterwards.
// It returns the reduced project, the reduced forced-service list, the step, and
// whether the full deployment must instead run in the applier.
func prepareSelfUpdate(
	ctx context.Context,
	dockerCli command.Cli,
	project *types.Project,
	deployConfig *deploy.Config,
	target *selfTarget,
	services []string,
	self *SelfDeployInput,
) (*types.Project, []string, func() error, bool, error) {
	opts := SelfUpdateConfig()

	log := slog.Default()
	if self != nil && self.Log != nil {
		log = self.Log
	}

	if !opts.Enabled {
		return nil, nil, nil, false, fmt.Errorf(
			"%w: stack %q contains this doco-cd instance (service %q); set SELF_UPDATE_ENABLED=true to let it update itself",
			selfupdate.ErrDisabled, project.Name, target.Service)
	}

	if opts.Store == nil {
		return nil, nil, nil, false, fmt.Errorf("%w: no self-update journal is configured", selfupdate.ErrUnsupported)
	}

	// One handover at a time. A record that is still being converged means the
	// stack has two containers, and a second attempt would race the first.
	active, err := opts.Store.Active()
	if err != nil {
		return nil, nil, nil, false, err
	}

	if active != nil {
		// Not a failure: the handover this instance is part of is still being
		// converged. Reporting it as one would record a failed deploy and make
		// the next poll force-recreate the stack.
		log.Info("self-update: a handover is still in progress, skipping this attempt",
			slog.String("id", active.ID),
			slog.String("state", string(active.State)),
		)

		return nil, nil, nil, false, selfupdate.ErrHandover
	}

	if err := validatePredecessorRestartPolicy(ctx, dockerCli.Client(), opts.Identity.ContainerID); err != nil {
		return nil, nil, nil, false, err
	}

	sourceType := ""
	if self != nil {
		sourceType = self.SourceType
	}

	strategy, drift, err := selectSelfStrategy(ctx, dockerCli, project, target, sourceType, log)
	if err != nil {
		return nil, nil, nil, false, err
	}

	if err := validateSelfUpdateVolumes(ctx, dockerCli.Client(), project); err != nil {
		return nil, nil, nil, false, err
	}

	// Disabled services are known to compose, so RemoveOrphans cannot reap the
	// self container while the rest of the project deploys.
	others := project.WithServicesDisabled(target.Service)

	reduced := make([]string, 0, len(services))

	for _, name := range services {
		if name != target.Service {
			reduced = append(reduced, name)
		}
	}

	step := func() error {
		return runSelfUpdate(ctx, dockerCli, project, deployConfig, target, strategy, drift, self, log)
	}

	return others, reduced, step, drift, nil
}

// runSelfUpdate performs the handover for the self service.
func runSelfUpdate(
	ctx context.Context,
	dockerCli command.Cli,
	project *types.Project,
	deployConfig *deploy.Config,
	target *selfTarget,
	strategy selfupdate.Strategy,
	drift bool,
	self *SelfDeployInput,
	log *slog.Logger,
) error {
	opts := SelfUpdateConfig()
	apiClient := dockerCli.Client()

	predecessor, err := apiClient.ContainerInspect(ctx, opts.Identity.ContainerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect own container: %w", err)
	}

	record := &selfupdate.Record{
		ID:       uuid.NewString(),
		State:    selfupdate.StateStaged,
		Strategy: strategy,
		Stack:    target.Project,
		Context:  target.Context,
		Service:  target.Service,
		Predecessor: selfupdate.ContainerRef{
			ID:     opts.Identity.ContainerID,
			Name:   opts.Identity.ContainerName,
			Number: opts.Identity.Number,
		},
		Source: selfSourceInfo(self),
		Deploy: selfupdate.DeployInfo{
			TimeoutSeconds: deployConfig.Timeout,
			RecreateMode:   api.RecreateForce,
			Services:       []string{target.Service},
			NetworkDrift:   drift,
		},
		Labels: successorLabels(project, target.Service, opts.Identity.Labels),
	}

	if err = opts.Store.Create(record); err != nil {
		return fmt.Errorf("create self-update record: %w", err)
	}

	if err = opts.Store.WriteSnapshot(record.ID, predecessor.Container); err != nil {
		err = fmt.Errorf("write self-update snapshot: %w", err)
	} else if drift {
		record.Drift, err = captureSelfDriftSnapshot(ctx, apiClient, project)
		if err == nil {
			if self != nil {
				record.Deploy.RecreateMode = self.RecreateMode
				record.Deploy.Services = self.Services
				record.Deploy.RemoveOrphans = self.RemoveOrphans
			} else {
				record.Deploy.RecreateMode = api.RecreateDiverged
				record.Deploy.Services = nil
			}

			err = opts.Store.Save(*record)
		}

		if err != nil {
			err = fmt.Errorf("snapshot self stack before network drift: %w", err)
		}
	}

	scaleOutAttempted := false

	if err == nil {
		switch strategy {
		case selfupdate.StrategyScaleOut:
			scaleOutAttempted = true
			err = selfUpdateScaleOut(ctx, dockerCli, project, deployConfig, target, record, log)
		case selfupdate.StrategyApplier:
			err = selfUpdateStageApplier(ctx, apiClient, record, log)
		default:
			err = fmt.Errorf("%w: strategy %q", selfupdate.ErrUnsupported, strategy)
		}
	}

	if err != nil {
		preserveRecord := false
		if scaleOutAttempted {
			preserveRecord, err = cleanupFailedScaleOut(ctx, apiClient, opts.Store, record, target, err)
		}

		if strategy == selfupdate.StrategyApplier && record.Applier.ID != "" {
			preserveRecord, err = cleanupFailedApplierStage(ctx, apiClient, opts.Store, record, err)
		}

		if !preserveRecord {
			if removeErr := opts.Store.RemoveIfUnchanged(*record); removeErr != nil {
				err = errors.Join(err, fmt.Errorf("remove failed self-update record: %w", removeErr))
			}
		}

		poisonErr := opts.Store.AddPoison(selfupdate.Poison{
			Context:     target.Context,
			Stack:       target.Project,
			CommitSHA:   record.Source.CommitSHA,
			ProjectHash: record.Source.ProjectHash,
			Reason:      err.Error(),
		})
		if poisonErr != nil {
			log.Warn("self-update: failed to record the poison entry", slog.Any("error", poisonErr))
		}

		if preserveRecord {
			// This only wakes recovery. The coordinator must check the
			// journal; neither a staging failure nor an Applying record
			// grants permission to drain a healthy predecessor.
			selfupdate.RequestDrain()
		}

		return err
	}

	selfupdate.RequestDrain()

	return selfupdate.ErrHandover
}

// cleanupFailedScaleOut only drops the journal after it can confirm there is
// no surviving candidate. Otherwise an aborted record lets the predecessor
// retry cleanup now or on its next boot.
func cleanupFailedScaleOut(
	ctx context.Context,
	apiClient client.APIClient,
	store *selfupdate.Store,
	record *selfupdate.Record,
	target *selfTarget,
	cause error,
) (bool, error) {
	cleanupCtx := context.WithoutCancel(ctx)
	if record.Successor.ID == "" {
		candidates, err := listSelfSuccessors(cleanupCtx, apiClient, target, record.Predecessor.ID)
		switch {
		case err != nil:
			cause = errors.Join(cause, fmt.Errorf("find failed scale-out candidates: %w", err))
		case len(candidates) == 1:
			record.Successor = candidates[0]
		case len(candidates) > 1:
			cause = errors.Join(cause, fmt.Errorf("found %d failed scale-out candidates; manual cleanup is unsafe", len(candidates)))
		default:
			return false, cause
		}
	}

	if record.Successor.ID != "" {
		err := removeContainerWithRetry(cleanupCtx, apiClient, record.Successor.ID)
		if err == nil {
			return false, cause
		}

		cause = errors.Join(cause, fmt.Errorf("remove failed scale-out successor %s: %w", record.Successor.ID, err))
	}

	record.Error = cause.Error()
	if err := store.Save(*record); err != nil {
		return true, errors.Join(cause, fmt.Errorf("save failed scale-out for recovery: %w", err))
	}

	updated, err := store.Update(*record, selfupdate.StateAborted, selfupdate.ActorPredecessor)
	if err != nil {
		return true, errors.Join(cause, fmt.Errorf("mark failed scale-out aborted: %w", err))
	}

	*record = updated

	return true, cause
}

// cleanupFailedApplierStage removes a clone even if recording or starting it
// failed. If removal remains uncertain, the journal retains its ID and a
// terminal failure so the healthy predecessor can retry cleanup.
func cleanupFailedApplierStage(
	ctx context.Context,
	apiClient client.APIClient,
	store *selfupdate.Store,
	record *selfupdate.Record,
	cause error,
) (bool, error) {
	removeErr := removeContainerWithRetry(context.WithoutCancel(ctx), apiClient, record.Applier.ID)
	if removeErr != nil {
		cause = errors.Join(cause, fmt.Errorf("remove failed applier %s: %w", record.Applier.ID, removeErr))
	}

	current, err := reloadUnchangedApplierRecord(store, record)
	if err != nil {
		return true, errors.Join(cause, err)
	}

	if removeErr == nil {
		return false, cause
	}

	current.Applier = record.Applier

	current.Error = cause.Error()
	if err = store.Save(current); err != nil {
		return true, errors.Join(cause, fmt.Errorf("save failed applier for recovery: %w", err))
	}

	to := selfupdate.StateAborted
	if current.State == selfupdate.StateApplying {
		to = selfupdate.StateFailed
	}

	updated, err := store.Update(current, to, selfupdate.ActorPredecessor)
	if err != nil {
		return true, errors.Join(cause, fmt.Errorf("mark failed applier for recovery: %w", err))
	}

	*record = updated

	return true, cause
}

// removeContainerWithRetry force-removes a container, treating an already
// removed one as success.
func removeContainerWithRetry(ctx context.Context, apiClient client.APIClient, id string) error {
	return selfupdate.Retry(ctx, func() error {
		_, err := apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
		if errdefs.IsNotFound(err) {
			return nil
		}

		return err
	}, nil)
}

// reloadUnchangedApplierRecord returns the persisted record unless another
// process advanced it or assigned another applier. In that case, record is
// replaced with the persisted version and the error wraps ErrStaleRecord.
func reloadUnchangedApplierRecord(store *selfupdate.Store, record *selfupdate.Record) (selfupdate.Record, error) {
	current, err := store.Load(record.ID)
	if err != nil {
		return current, fmt.Errorf("reload journal for applier cleanup: %w", err)
	}

	if !current.SameProgress(*record) {
		*record = current
		return current, fmt.Errorf("%w: applier journal changed during clone cleanup", selfupdate.ErrStaleRecord)
	}

	if current.Applier.ID != "" && current.Applier.ID != record.Applier.ID {
		*record = current
		return current, fmt.Errorf("%w: journal tracks another applier %s", selfupdate.ErrStaleRecord, current.Applier.ID)
	}

	return current, nil
}

// successorLabels returns the labels the successor is deployed with. Sources are
// immutable per revision now, so the predecessor's own labels still point at the
// artifact it was started from; the applier must reload the new one instead.
func successorLabels(project *types.Project, service string, fallback map[string]string) map[string]string {
	if svc, ok := project.Services[service]; ok && len(svc.CustomLabels) > 0 {
		return maps.Clone(svc.CustomLabels)
	}

	return fallback
}

// selfUpdateScaleOut creates a second container from the new config, waits for
// it to become healthy, then hands over. The predecessor stays alive and fully
// functional until the successor proves itself, so a rollback costs one remove.
func selfUpdateScaleOut(
	ctx context.Context,
	dockerCli command.Cli,
	project *types.Project,
	deployConfig *deploy.Config,
	target *selfTarget,
	record *selfupdate.Record,
	log *slog.Logger,
) error {
	opts := SelfUpdateConfig()
	apiClient := dockerCli.Client()

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	scaleProject := *project
	scaleProject.Services = maps.Clone(project.Services)

	svc := scaleProject.Services[target.Service]
	two := 2
	svc.Scale = &two
	scaleProject.Services[target.Service] = svc

	if err := validateSelfUpdateVolumes(ctx, apiClient, &scaleProject); err != nil {
		return err
	}

	// RecreateNever leaves the diverged predecessor untouched, so compose only
	// plans the scale-up node and creates container #2 with the new config.
	err = service.Create(ctx, &scaleProject, api.CreateOptions{
		Services:             []string{target.Service},
		Recreate:             api.RecreateNever,
		RecreateDependencies: api.RecreateNever,
		IgnoreOrphans:        true,
		QuietPull:            true,
	})
	if err != nil {
		return fmt.Errorf("create successor container: %w", err)
	}

	successor, err := findSelfSuccessor(ctx, apiClient, target, opts.Identity.ContainerID)
	if err != nil {
		return err
	}

	record.Successor = successor

	if err = opts.Store.Save(*record); err != nil {
		return fmt.Errorf("record successor: %w", err)
	}

	if _, err = apiClient.ContainerStart(ctx, successor.ID, client.ContainerStartOptions{}); err != nil {
		return removeSuccessorAndFail(ctx, apiClient, successor.ID, fmt.Errorf("start successor: %w", err), log)
	}

	updated, err := opts.Store.Update(*record, selfupdate.StateStarted, selfupdate.ActorPredecessor)
	if err != nil {
		return err
	}

	*record = updated

	selfupdate.MaybeCrash(opts.DataMountPath, string(selfupdate.StateStarted), log)

	log.Info("self-update: successor started, waiting for health",
		slog.String("successor_id", successor.ID),
		slog.Int("timeout_seconds", deployConfig.Timeout),
	)

	timeout := time.Duration(deployConfig.Timeout) * time.Second
	if timeout <= 0 {
		timeout = defaultSelfHealthTimeout
	}

	if err = selfupdate.WaitHealthy(ctx, apiClient, successor.ID, timeout, log); err != nil {
		return removeSuccessorAndFail(ctx, apiClient, successor.ID, err, log)
	}

	updated, err = opts.Store.Update(*record, selfupdate.StateHandover, selfupdate.ActorPredecessor)
	if err != nil {
		return err
	}

	*record = updated

	log.Info("self-update: successor healthy, handing over", slog.String("successor_id", successor.ID))

	selfupdate.MaybeCrash(opts.DataMountPath, string(selfupdate.StateHandover), log)

	return nil
}

// removeSuccessorAndFail rolls a failed scale-out back. Nothing was stopped, so
// removing the successor restores the state from before the attempt.
func removeSuccessorAndFail(ctx context.Context, apiClient client.APIClient, successorID string, cause error, log *slog.Logger) error {
	removeCtx := context.WithoutCancel(ctx)

	if _, err := apiClient.ContainerRemove(removeCtx, successorID, client.ContainerRemoveOptions{Force: true}); err != nil {
		log.Warn("self-update: failed to remove the unhealthy successor",
			slog.String("successor_id", successorID), slog.Any("error", err))
	}

	return fmt.Errorf("self-update rolled back, this instance keeps running: %w", cause)
}

// findSelfSuccessor returns the project container of the self service that is
// not this instance.
func findSelfSuccessor(ctx context.Context, apiClient client.APIClient, target *selfTarget, ownID string) (selfupdate.ContainerRef, error) {
	found, err := listSelfSuccessors(ctx, apiClient, target, ownID)
	if err != nil {
		return selfupdate.ContainerRef{}, err
	}

	if len(found) != 1 {
		return selfupdate.ContainerRef{}, fmt.Errorf("expected exactly one successor container, found %d", len(found))
	}

	return found[0], nil
}

// listSelfSuccessors finds replacement containers for the self service while
// excluding the running predecessor.
func listSelfSuccessors(ctx context.Context, apiClient client.APIClient, target *selfTarget, ownID string) ([]selfupdate.ContainerRef, error) {
	list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
		All: true,
		Filters: make(client.Filters).
			Add("label", api.ProjectLabel+"="+target.Project).
			Add("label", api.ServiceLabel+"="+target.Service),
	})
	if err != nil {
		return nil, fmt.Errorf("list self service containers: %w", err)
	}

	var found []selfupdate.ContainerRef

	for _, c := range list.Items {
		if c.ID != ownID {
			ref := selfupdate.ContainerRef{ID: c.ID}
			if len(c.Names) > 0 {
				ref.Name = c.Names[0]
				if len(ref.Name) > 0 && ref.Name[0] == '/' {
					ref.Name = ref.Name[1:]
				}
			}

			if number, convErr := strconv.Atoi(c.Labels[api.ContainerNumberLabel]); convErr == nil {
				ref.Number = number
			}

			found = append(found, ref)
		}
	}

	return found, nil
}

func selfSourceInfo(self *SelfDeployInput) selfupdate.SourceInfo {
	if self == nil {
		return selfupdate.SourceInfo{}
	}

	return selfupdate.SourceInfo{
		RepoName:     self.RepoName,
		SourceURL:    self.SourceURL,
		FullName:     self.FullName,
		SourceType:   self.SourceType,
		Reference:    self.Reference,
		ConfigTarget: self.ConfigTarget,
		CommitSHA:    self.CommitSHA,
		ProjectHash:  self.ProjectHash,
		JobID:        self.JobID,
		Trigger:      self.Trigger,
	}
}
