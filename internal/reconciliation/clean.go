package reconciliation

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"

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

	for _, cfg := range deployConfigs {
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
			stackLog.Debug("checking auto-discovered stack for obsolescence")

			if _, found := autoDiscoveredNames[stackName]; !found {
				autoDiscoverDelete := labels[docker.DocoCDLabels.Deployment.AutoDiscoveryDelete]
				if autoDiscoverDelete == "" {
					// Fall back to deprecated label
					autoDiscoverDelete = labels[docker.DeprecatedAutoDiscoverDeleteLabel] //nolint:staticcheck // fallback for pre-rename containers
				}

				if autoDiscoverDelete == "" {
					autoDiscoverDelete = "true" // Default to true if label is missing
				}

				deleteEnabled, err := strconv.ParseBool(autoDiscoverDelete)
				if err != nil {
					return err
				}

				if !deleteEnabled {
					stackLog.Debug("skipping removal of obsolete auto-discovered stack as per configuration")

					processedStacks = append(processedStacks, stackName)

					continue
				}

				stackLog.Info("removing obsolete auto-discovered stack")

				removeConfig := &deployConfig.Config{Name: stackName}
				removeConfig.Destroy.Enabled = true
				removeConfig.Destroy.RemoveVolumes = true
				removeConfig.Destroy.RemoveImages = true
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
