package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
// It reports whether previous-mode resources were removed.
func MigrateDeploymentMode(ctx context.Context, log *slog.Logger, dockerCli command.Cli, contextName, stackName, source string, swarmMode, swarmAvailable bool) (bool, error) {
	if dockerCli == nil {
		return false, errors.New("docker cli is required")
	}

	// Without Swarm capability there is no second mode to migrate between: the
	// context is either a standalone engine or has Swarm features globally
	// disabled, in which case its Swarm API must not be probed at all.
	if !swarmAvailable {
		return false, nil
	}

	stackLockKey := lock.StackKey(contextName, stackName)

	lock.LockStack(stackLockKey)
	defer lock.UnlockStack(stackLockKey)

	previousMode := !swarmMode

	labelsByService, err := deploymentModeLabels(ctx, dockerCli.Client(), stackName, previousMode)
	if err != nil {
		return false, fmt.Errorf("failed to inspect %s resources for deployment mode migration: %w", deploymentModeName(previousMode), err)
	}

	if len(labelsByService) == 0 {
		return false, nil
	}

	expectedSources := migrationSourceCandidates(source)
	if err = validateMigrationOwnership(labelsByService, expectedSources, previousMode, stackName); err != nil {
		return false, err
	}

	// A partial earlier migration may have resources in both modes. Do not
	// remove the verified old mode if the selected mode is occupied by an
	// unmanaged same-named deployment.
	selectedLabels, err := deploymentModeLabels(ctx, dockerCli.Client(), stackName, swarmMode)
	if err != nil {
		return false, fmt.Errorf("failed to inspect %s resources for deployment mode migration: %w", deploymentModeName(swarmMode), err)
	}

	if err := validateMigrationOwnership(selectedLabels, expectedSources, swarmMode, stackName); err != nil {
		return false, err
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
		return false, fmt.Errorf("failed to remove previous %s deployment: %w", deploymentModeName(previousMode), err)
	}

	return true, nil
}

// deploymentModeLabels returns a map of service names to their labels for the given stack in the specified deployment mode.
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

// validateMigrationOwnership checks that all discovered resources are owned by this deployment source.
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

// migrationSourceMatches returns true if any of the source labels match the expected sources.
func migrationSourceMatches(labels Labels, expectedSources map[string]struct{}) bool {
	for _, source := range []string{
		labels[DocoCDLabels.Source.Name],
		labels[DocoCDLabels.Source.URL],
	} {
		for candidate := range migrationSourceCandidates(source) {
			if _, ok := expectedSources[candidate]; ok {
				return true
			}
		}
	}

	return false
}

// migrationSourceCandidates returns a set of normalized source candidates for matching against existing resources.
func migrationSourceCandidates(source string) map[string]struct{} {
	normalized := normalizeRepositoryForLabelMatch(source)
	candidates := map[string]struct{}{
		normalized: {},
	}

	source = strings.TrimSpace(source)
	if strings.Contains(source, "://") ||
		(strings.Contains(source, "@") && strings.Contains(source, ":")) ||
		hasRepositoryHostPrefix(normalized) {
		candidates[normalizeRepositoryForLabelMatch(git.GetFullName(normalized))] = struct{}{}
	}

	return candidates
}

func hasRepositoryHostPrefix(repository string) bool {
	parts := strings.Split(repository, "/")
	if len(parts) < 3 {
		return false
	}

	host := parts[0]

	return host == "localhost" || strings.ContainsAny(host, ".:")
}

// deploymentModeName returns a human-readable name for the deployment mode.
func deploymentModeName(swarmMode bool) string {
	if swarmMode {
		return "swarm"
	}

	return "compose"
}
