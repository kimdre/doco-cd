package reconciliation

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"

	"github.com/kimdre/doco-cd/internal/notification"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
)

// CleanupObsoleteAutoDiscoveredContainers removes obsolete auto-discovered containers that are no longer defined in
// the current deployment configurations but still exist on the Docker host.
func CleanupObsoleteAutoDiscoveredContainers(ctx context.Context, jobLog *slog.Logger,
	dockerClient client.APIClient, dockerCli command.Cli,
	cloneUrl string, deployConfigs []*config.DeployConfig, metadata notification.Metadata,
) error {
	autoDiscoveredNames := make(map[string]bool)

	for _, cfg := range deployConfigs {
		if cfg.AutoDiscover {
			autoDiscoveredNames[cfg.Name] = cfg.AutoDiscoverOpts.Delete
		}
	}

	jobLog = jobLog.With(slog.String("repo_clone_url", cloneUrl))

	var processedStacks []string

	serviceLabels, err := docker.GetLabeledServices(ctx, dockerClient, docker.DocoCDLabels.Deployment.AutoDiscover, "true")
	if err == nil {
		for _, labels := range serviceLabels {
			stackName := labels[docker.DocoCDLabels.Deployment.Name]

			// Skip container if it has already been removed in this cleanup run
			if slices.Contains(processedStacks, stackName) {
				continue
			}

			stackLog := jobLog.With(slog.String("stack", stackName))

			labelUrl := labels[docker.DocoCDLabels.Repository.URL]

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
					autoDiscoverDelete := labels[docker.DocoCDLabels.Deployment.AutoDiscoverDelete]
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

					removeConfig := &config.DeployConfig{Name: stackName, Destroy: true}
					removeConfig.DestroyOpts.RemoveVolumes = true
					removeConfig.DestroyOpts.RemoveImages = true
					removeConfig.DestroyOpts.RemoveRepoDir = false // Do not remove repo dir for auto-discovered stacks

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
	} else {
		return fmt.Errorf("failed to retrieve containers for auto-discovery cleanup: %w", err)
	}

	return nil
}
