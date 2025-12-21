package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/client"

	"github.com/kimdre/doco-cd/internal/logger"

	"github.com/kimdre/doco-cd/internal/notification"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
)

// getAppContainerID retrieves the application container ID from the cgroup mounts.
func getAppContainerID() (string, error) {
	const cgroupMounts = "/proc/self/mountinfo"

	data, err := os.ReadFile(cgroupMounts)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", cgroupMounts, err)
	}

	id := extractContainerIDFromMountInfo(string(data))
	if id != "" {
		return id, nil
	}

	return "", docker.ErrContainerIDNotFound
}

// extractContainerIDFromMountInfo extracts the container ID from the mount info content.
func extractContainerIDFromMountInfo(content string) string {
	containerIdPattern := regexp.MustCompile(`[a-z0-9]{64}`)

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		mountPath := fields[3]

		if strings.Contains(line, "/etc/hostname") {
			if matches := containerIdPattern.FindStringSubmatch(mountPath); len(matches) > 0 {
				return matches[0]
			}
		}
	}

	return ""
}

// cleanupObsoleteAutoDiscoveredContainers removes obsolete auto-discovered containers that are no longer defined in
// the current deployment configurations but still exist on the Docker host.
func cleanupObsoleteAutoDiscoveredContainers(ctx context.Context, jobLog *slog.Logger,
	dockerClient *client.Client, dockerCli command.Cli,
	cloneUrl string, deployConfigs []*config.DeployConfig, metadata notification.Metadata,
) error {
	autoDiscoveredNames := make(map[string]bool)

	for _, cfg := range deployConfigs {
		if cfg.AutoDiscover {
			autoDiscoveredNames[cfg.Name] = cfg.AutoDiscoverOpts.Delete
		}
	}

	var processedStacks []string

	serviceLabels, err := docker.GetLabeledServices(ctx, dockerClient, docker.DocoCDLabels.Deployment.AutoDiscover, "true")
	if err == nil {
		for _, labels := range serviceLabels {
			stackName := labels[docker.DocoCDLabels.Deployment.Name]

			// Skip container if it has already been removed in this cleanup run
			if slices.Contains(processedStacks, stackName) {
				continue
			}

			if cloneUrl == labels[docker.DocoCDLabels.Repository.URL] {
				jobLog.Debug("checking auto-discovered stack for obsolescence", slog.String("stack", stackName))

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
						jobLog.Debug("skipping removal of obsolete auto-discovered stack as per configuration", slog.String("stack", stackName))
						processedStacks = append(processedStacks, stackName)

						continue
					}

					jobLog.Info("removing obsolete auto-discovered stack", slog.String("stack", stackName))
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
						jobLog.Error("failed to send notification", logger.ErrAttr(err))
					}

					jobLog.Info("removed obsolete auto-discovered stack", slog.String("stack", stackName))
					processedStacks = append(processedStacks, stackName)
				}
			}
		}
	} else {
		return fmt.Errorf("failed to retrieve containers for auto-discovery cleanup: %w", err)
	}

	return nil
}
