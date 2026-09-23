package docker

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
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
// It returns the reduced project, the reduced forced-service list, and the step.
func prepareSelfUpdate(
	ctx context.Context,
	dockerCli command.Cli,
	project *types.Project,
	deployConfig *deploy.Config,
	target *selfTarget,
	services []string,
	self *SelfDeployInput,
) (*types.Project, []string, func() error, error) {
	opts := SelfUpdateConfig()

	log := slog.Default()
	if self != nil && self.Log != nil {
		log = self.Log
	}

	if !opts.Enabled {
		return nil, nil, nil, fmt.Errorf(
			"%w: stack %q contains this doco-cd instance (service %q); set SELF_UPDATE_ENABLED=true to let it update itself",
			selfupdate.ErrDisabled, project.Name, target.Service)
	}

	if opts.Store == nil {
		return nil, nil, nil, fmt.Errorf("%w: no self-update journal is configured", selfupdate.ErrUnsupported)
	}

	// One handover at a time. A record that is still being converged means the
	// stack has two containers, and a second attempt would race the first.
	active, err := opts.Store.Active()
	if err != nil {
		return nil, nil, nil, err
	}

	if active != nil {
		// Not a failure: the handover this instance is part of is still being
		// converged. Reporting it as one would record a failed deploy and make
		// the next poll force-recreate the stack.
		log.Info("self-update: a handover is still in progress, skipping this attempt",
			slog.String("id", active.ID),
			slog.String("state", string(active.State)),
		)

		return nil, nil, nil, selfupdate.ErrHandover
	}

	sourceType := ""
	if self != nil {
		sourceType = self.SourceType
	}

	strategy, err := selectSelfStrategy(ctx, dockerCli, project, target, sourceType, log)
	if err != nil {
		return nil, nil, nil, err
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
		return runSelfUpdate(ctx, dockerCli, project, deployConfig, target, strategy, self, log)
	}

	return others, reduced, step, nil
}

// runSelfUpdate performs the handover for the self service.
func runSelfUpdate(
	ctx context.Context,
	dockerCli command.Cli,
	project *types.Project,
	deployConfig *deploy.Config,
	target *selfTarget,
	strategy selfupdate.Strategy,
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
		},
		Labels: successorLabels(project, target.Service, opts.Identity.Labels),
	}

	if err = opts.Store.Create(record); err != nil {
		return fmt.Errorf("create self-update record: %w", err)
	}

	if err = opts.Store.WriteSnapshot(record.ID, predecessor.Container); err != nil {
		return fmt.Errorf("write self-update snapshot: %w", err)
	}

	switch strategy {
	case selfupdate.StrategyScaleOut:
		err = selfUpdateScaleOut(ctx, dockerCli, project, deployConfig, target, record, log)
	case selfupdate.StrategyApplier:
		err = selfUpdateStageApplier(ctx, apiClient, record, log)
	default:
		err = fmt.Errorf("%w: strategy %q", selfupdate.ErrUnsupported, strategy)
	}

	if err != nil {
		_ = opts.Store.Remove(record.ID)

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

		return err
	}

	selfupdate.RequestDrain()

	return selfupdate.ErrHandover
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
	list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
		All: true,
		Filters: make(client.Filters).
			Add("label", api.ProjectLabel+"="+target.Project).
			Add("label", api.ServiceLabel+"="+target.Service),
	})
	if err != nil {
		return selfupdate.ContainerRef{}, fmt.Errorf("list self service containers: %w", err)
	}

	var found []container.Summary

	for _, c := range list.Items {
		if c.ID != ownID {
			found = append(found, c)
		}
	}

	if len(found) != 1 {
		return selfupdate.ContainerRef{}, fmt.Errorf("expected exactly one successor container, found %d", len(found))
	}

	c := found[0]
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

	return ref, nil
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
