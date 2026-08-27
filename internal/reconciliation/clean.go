package reconciliation

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/docker/cli/cli/command"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"

	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"

	"github.com/kimdre/doco-cd/internal/notification"

	"github.com/kimdre/doco-cd/internal/docker"
)

// cleanupObsoleteAutoDiscoveredContainers removes obsolete auto-discovered containers that are no longer defined in
// the current deployment configurations but still exist on the Docker host.
// contextName is the Docker context dockerCli is connected to and is only used to attribute
// notifications, since the same stack name can exist on several contexts.
func cleanupObsoleteAutoDiscoveredContainers(ctx context.Context, jobLog *slog.Logger,
	dockerCli command.Cli, swarmMode bool, contextName string,
	cloneUrl string, deployConfigs []*deployConfig.Config, metadata notification.Metadata,
) error {
	autoDiscoveredNames := make(map[string]bool)
	runConfigTargets := make(map[string]struct{})

	for _, cfg := range deployConfigs {
		runConfigTargets[strings.TrimSpace(cfg.Internal.ConfigTarget)] = struct{}{}

		if cfg.AutoDiscovery.Enabled {
			autoDiscoveredNames[cfg.Name] = cfg.AutoDiscovery.Delete
		}
	}

	jobLog = jobLog.With(slog.String("repo_clone_url", cloneUrl))

	var processedStacks []string

	serviceLabels, err := docker.GetAutoDiscoveryServices(ctx, dockerCli.Client(), swarmMode)
	if err != nil {
		if serviceLabels == nil {
			return fmt.Errorf("failed to retrieve containers for auto-discovery cleanup: %w", err)
		}

		jobLog.Warn("failed to migrate auto-discovery labels for some services; continuing cleanup",
			logger.ErrAttr(err),
		)
	}

	for _, labels := range serviceLabels {
		stackName := labels[docker.DocoCDLabels.Deployment.Name]

		// Skip container if it has already been removed in this cleanup run
		if slices.Contains(processedStacks, stackName) {
			continue
		}

		stackLog := jobLog.With(slog.String("stack", stackName))

		labelUrl := labels[docker.DocoCDLabels.Source.URL]

		// cloneUrl may not be in the same format as labelUrl
		//  (e.g., "https://github.com/kimdre/doco-cd.git" vs. "https://github.com/kimdre/doco-cd")
		// or my different protocols (e.g., "ssh://git@github.com/kimdre/doco-cd.git" vs. "https://github.com/kimdre/doco-cd")
		cloneUrlRepoName := git.GetRepoName(cloneUrl)
		labelUrlRepoName := git.GetRepoName(labelUrl)

		match := cloneUrlRepoName == labelUrlRepoName

		stackLog.Debug("checking auto-discovered stack for repository match",
			slog.Group("repo_url",
				slog.String("clone_url", cloneUrl),
				slog.String("clone_url_repo_name", cloneUrlRepoName),
				slog.String("label_url", labelUrl),
				slog.String("label_url_repo_name", labelUrlRepoName),
			),
			slog.Bool("match", match),
		)

		if match {
			if _, found := autoDiscoveredNames[stackName]; found {
				stackLog.Debug("auto-discovered stack is present in current config, skipping obsolete cleanup")

				processedStacks = append(processedStacks, stackName)

				continue
			}

			stackConfigTarget := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.ConfigTarget])
			if !isCleanupTargetMatch(runConfigTargets, stackConfigTarget) {
				stackLog.Debug("skipping auto-discovered stack as it belongs to a different deployment config target",
					slog.String("stack_config_target", stackConfigTarget),
					slog.Any("run_config_targets", sortedTargetKeys(runConfigTargets)),
				)

				continue
			}

			stackLog.Debug("checking auto-discovered stack for obsolescence")

			autoDiscoverCfg := docker.ParseAutoDiscoveryConfig(labels[docker.DocoCDLabels.Deployment.AutoDiscoveryConfig])

			if !autoDiscoverCfg.Delete {
				stackLog.Debug("skipping removal of obsolete auto-discovered stack as per configuration")

				processedStacks = append(processedStacks, stackName)

				continue
			}

			stackLog.Info("removing obsolete auto-discovered stack")

			notifyMetadata := metadata
			notifyMetadata.Target = stackConfigTarget
			notifyMetadata.Stack = stackName
			notifyMetadata.Context = contextName

			removeConfig := &deployConfig.Config{Name: stackName}
			removeConfig.Destroy.Enabled = true
			removeConfig.Destroy.RemoveVolumes = autoDiscoverCfg.RemoveVolumes
			removeConfig.Destroy.RemoveImages = autoDiscoverCfg.RemoveImages
			removeConfig.Destroy.RemoveRepoDir = false // Do not remove repo dir for auto-discovered stacks

			err = docker.DestroyStack(jobLog, &ctx, &dockerCli, removeConfig, swarmMode)
			if err != nil {
				return fmt.Errorf("failed to remove obsolete auto-discovered stack '%s': %w", stackName, err)
			}

			err = notification.Send(notification.Success, "Stack destroyed", "successfully destroyed stack "+removeConfig.Name, notifyMetadata)
			if err != nil {
				stackLog.Error("failed to send notification", logger.ErrAttr(err))
			}

			stackLog.Info("removed obsolete auto-discovered stack", slog.String("stack", stackName))
			processedStacks = append(processedStacks, stackName)
		} else {
			stackLog.Debug("skipping auto-discovered stack as it belongs to a different repository")
		}
	}

	return nil
}

// isCleanupTargetMatch checks if the stack's config target matches any of the run config targets.
func isCleanupTargetMatch(runConfigTargets map[string]struct{}, stackConfigTarget string) bool {
	// Backward compatibility: if no run target context is available, keep legacy behavior.
	if len(runConfigTargets) == 0 {
		return true
	}

	stackConfigTarget = strings.TrimSpace(stackConfigTarget)

	// Backward compatibility for pre-label deployments: only include unlabeled stacks
	// for default-target runs, never for custom targets.
	if stackConfigTarget == "" {
		_, defaultTargetRun := runConfigTargets[""]
		return defaultTargetRun
	}

	_, ok := runConfigTargets[stackConfigTarget]

	return ok
}

func sortedTargetKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}
