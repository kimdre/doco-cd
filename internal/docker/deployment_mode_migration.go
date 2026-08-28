package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/lock"
)

// ErrDeploymentModeConflict means resources in the old deployment mode cannot
// safely be replaced because doco-cd cannot prove that it owns them.
var ErrDeploymentModeConflict = errors.New("deployment mode migration conflicts with existing resources")

// MigrateDeploymentMode removes a previous deployment in the other runtime mode,
// but only after proving every discovered resource belongs to this doco-cd deployment.
// Volumes are deliberately retained for the new mode.
func MigrateDeploymentMode(ctx context.Context, log *slog.Logger, dockerCli command.Cli, contextName, stackName, source string, swarmMode, swarmAvailable bool) error {
	if dockerCli == nil {
		return errors.New("docker cli is required")
	}

	// Without Swarm capability there is no second mode to migrate between: the
	// context is either a standalone engine or has Swarm features globally
	// disabled, in which case its Swarm API must not be probed at all.
	if !swarmAvailable {
		return nil
	}

	stackLockKey := lock.StackKey(contextName, stackName)

	lock.LockStack(stackLockKey)
	defer lock.UnlockStack(stackLockKey)

	previousMode := !swarmMode

	labelsByService, err := deploymentModeLabels(ctx, dockerCli.Client(), stackName, previousMode)
	if err != nil {
		return fmt.Errorf("failed to inspect %s resources for deployment mode migration: %w", deploymentModeName(previousMode), err)
	}

	if len(labelsByService) == 0 {
		return nil
	}

	expectedSources := buildRepositoryLabelCandidates(source)
	if err = validateMigrationOwnership(labelsByService, expectedSources, previousMode, stackName); err != nil {
		return err
	}

	// A partial earlier migration may have resources in both modes. Do not
	// remove the verified old mode if the selected mode is occupied by an
	// unmanaged same-named deployment.
	selectedLabels, err := deploymentModeLabels(ctx, dockerCli.Client(), stackName, swarmMode)
	if err != nil {
		return fmt.Errorf("failed to inspect %s resources for deployment mode migration: %w", deploymentModeName(swarmMode), err)
	}

	if err := validateMigrationOwnership(selectedLabels, expectedSources, swarmMode, stackName); err != nil {
		return err
	}

	if log == nil {
		log = slog.Default()
	}

	log.Info("removing previous deployment mode before migration",
		slog.String("stack", stackName),
		slog.String("from", deploymentModeName(previousMode)),
		slog.String("to", deploymentModeName(swarmMode)),
	)

	removeConfig := deployConfig.New(stackName, "")
	removeConfig.RemoveOrphans = true
	removeConfig.Destroy.Enabled = true
	removeConfig.Destroy.RemoveVolumes = false
	removeConfig.Destroy.RemoveImages = false

	if err := DestroyStack(log, &ctx, &dockerCli, removeConfig, previousMode); err != nil {
		return fmt.Errorf("failed to remove previous %s deployment: %w", deploymentModeName(previousMode), err)
	}

	return nil
}

func deploymentModeLabels(ctx context.Context, dockerClient client.APIClient, stackName string, swarmMode bool) (map[Service]Labels, error) {
	if swarmMode {
		return GetServiceLabels(ctx, dockerClient, true, stackName)
	}

	containers, err := GetLabeledContainers(ctx, dockerClient, api.ProjectLabel, stackName, true)
	if err != nil {
		return nil, err
	}

	labels := make(map[Service]Labels, len(containers))
	for _, container := range containers {
		name := container.ID
		if len(container.Names) > 0 {
			name = container.Names[0]
		}

		labels[Service(name)] = container.Labels
	}

	return labels, nil
}

func validateMigrationOwnership(labelsByService map[Service]Labels, expectedSources map[string]struct{}, mode bool, stackName string) error {
	for service, labels := range labelsByService {
		if labels[DocoCDLabels.Metadata.Manager] != app.Name ||
			!migrationSourceMatches(labels, expectedSources) {
			return fmt.Errorf("%w: %s %q exists for stack %q and is not managed by this deployment source",
				ErrDeploymentModeConflict, deploymentModeName(mode), service, stackName)
		}
	}

	return nil
}

func migrationSourceMatches(labels Labels, expectedSources map[string]struct{}) bool {
	for _, source := range []string{
		labels[DocoCDLabels.Source.Name],
		labels[DocoCDLabels.Source.URL],
	} {
		source = normalizeRepositoryForLabelMatch(source)
		if _, ok := expectedSources[source]; ok {
			return true
		}

		if _, ok := expectedSources[git.GetFullName(source)]; ok {
			return true
		}
	}

	return false
}

func deploymentModeName(swarmMode bool) string {
	if swarmMode {
		return "swarm"
	}

	return "compose"
}
