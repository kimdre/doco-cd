package reconciliation

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
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
func cleanupObsoleteAutoDiscoveredContainers(ctx context.Context, jobLog *slog.Logger,
	dockerCli command.Cli,
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

	// Query both new and deprecated labels. We keep reading the deprecated label to
	// handle containers deployed before the label rename.
	newServiceLabels, err := docker.GetLabeledServices(ctx, dockerCli.Client(), docker.DocoCDLabels.Deployment.AutoDiscovery, "true")
	if err != nil {
		return fmt.Errorf("failed to retrieve containers for auto-discovery cleanup: %w", err)
	}

	deprecatedServiceLabels, err := docker.GetLabeledServices(ctx, dockerCli.Client(), docker.DeprecatedAutoDiscoverLabel, "true") //nolint:staticcheck // fallback for pre-rename containers
	if err != nil {
		return fmt.Errorf("failed to retrieve containers for auto-discovery cleanup: %w", err)
	}

	if len(deprecatedServiceLabels) > 0 {
		jobLog.Warn("found containers with deprecated label, please recreate them to migrate to the new label",
			slog.String("deprecated_label", docker.DeprecatedAutoDiscoverLabel), //nolint:staticcheck // include deprecated label key in warning for migration clarity
			slog.String("new_label", docker.DocoCDLabels.Deployment.AutoDiscovery),
		)
	}

	// Merge label maps and prefer the new label set when a service appears in both.
	serviceLabels := make(map[docker.Service]map[string]string, len(deprecatedServiceLabels)+len(newServiceLabels))
	maps.Copy(serviceLabels, deprecatedServiceLabels)

	maps.Copy(serviceLabels, newServiceLabels)

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
			stackConfigTarget := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.ConfigTarget])
			if !isCleanupTargetMatch(runConfigTargets, stackConfigTarget) {
				stackLog.Debug("skipping auto-discovered stack as it belongs to a different deployment config target",
					slog.String("stack_config_target", stackConfigTarget),
					slog.Any("run_config_targets", sortedTargetKeys(runConfigTargets)),
				)

				continue
			}

			stackLog.Debug("checking auto-discovered stack for obsolescence")

			if _, found := autoDiscoveredNames[stackName]; !found {
				// Parse the auto-discovery config from the new JSON label.
				// Fall back to the old scalar labels for containers deployed before this change.
				autoDiscoverCfg := docker.ParseAutoDiscoveryConfig(labels[docker.DocoCDLabels.Deployment.AutoDiscoveryConfig])

				// If the new label was absent, try the legacy scalar delete label.
				if labels[docker.DocoCDLabels.Deployment.AutoDiscoveryConfig] == "" {
					legacyDelete := labels[docker.DeprecatedAutoDiscoveryDeleteLabel] //nolint:staticcheck // fallback for pre-consolidation containers
					if legacyDelete == "" {
						legacyDelete = labels[docker.DeprecatedAutoDiscoverDeleteLabel] //nolint:staticcheck // fallback for pre-rename containers
					}

					if legacyDelete != "" {
						if parsed, err := strconv.ParseBool(legacyDelete); err == nil {
							autoDiscoverCfg.Delete = parsed
						}
					}
				}

				if !autoDiscoverCfg.Delete {
					stackLog.Debug("skipping removal of obsolete auto-discovered stack as per configuration")

					processedStacks = append(processedStacks, stackName)

					continue
				}

				stackLog.Info("removing obsolete auto-discovered stack")

				removeConfig := &deployConfig.Config{Name: stackName}
				removeConfig.Destroy.Enabled = true
				removeConfig.Destroy.RemoveVolumes = autoDiscoverCfg.RemoveVolumes
				removeConfig.Destroy.RemoveImages = autoDiscoverCfg.RemoveImages
				removeConfig.Destroy.RemoveRepoDir = false // Do not remove repo dir for auto-discovered stacks

				err = docker.DestroyStack(jobLog, &ctx, &dockerCli, removeConfig)
				if err != nil {
					return fmt.Errorf("failed to remove obsolete auto-discovered stack '%s': %w", stackName, err)
				}

				err = notification.Send(notification.Success, "Stack destroyed", "successfully destroyed stack "+removeConfig.Name, metadata)
				if err != nil {
					stackLog.Error("failed to send notification", logger.ErrAttr(err))
				}

				stackLog.Info("removed obsolete auto-discovered stack", slog.String("stack", stackName))
				processedStacks = append(processedStacks, stackName)
			}
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
