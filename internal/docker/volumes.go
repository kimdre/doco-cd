package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

// volumeConfigMatch checks if two volume configurations match
// Returns true if the volumes have the same driver and driver options.
func volumeConfigMatch(existing, desired *types.VolumeConfig) bool {
	if existing == nil || desired == nil {
		return existing == desired
	}

	// Check if driver matches (empty string defaults to "local")
	existingDriver := existing.Driver
	if existingDriver == "" {
		existingDriver = "local"
	}

	desiredDriver := desired.Driver
	if desiredDriver == "" {
		desiredDriver = "local"
	}

	if existingDriver != desiredDriver {
		return false
	}

	// Check if driver options match
	existingOpts := existing.DriverOpts
	if existingOpts == nil {
		existingOpts = make(types.Options)
	}

	desiredOpts := desired.DriverOpts
	if desiredOpts == nil {
		desiredOpts = make(types.Options)
	}

	// Compare each option as a string representation
	// This handles the case where options might have different types but same values
	for k, v := range desiredOpts {
		existingVal, exists := existingOpts[k]
		if !exists {
			return false
		}
		// Convert to string for comparison to handle type differences
		if existingVal != v {
			return false
		}
	}

	// Check if any existing options are not in desired
	for k := range existingOpts {
		if _, exists := desiredOpts[k]; !exists {
			return false
		}
	}

	return true
}

func isRecreatableVolumeType(cfg *types.VolumeConfig) bool {
	if cfg == nil {
		return false
	}

	driver := strings.ToLower(strings.TrimSpace(cfg.Driver))

	if driver != "" && driver != "local" {
		return false
	}

	volType := strings.ToLower(strings.TrimSpace(cfg.DriverOpts["type"]))

	return volType == "nfs" || volType == "cifs" || volType == "tmpfs"
}

func getContainersUsingVolume(ctx context.Context, apiClient client.APIClient, stackName, volumeName string) ([]container.Summary, error) {
	labelKeys := []string{api.ProjectLabel, swarm.StackNamespaceLabel}
	containerByID := make(map[string]container.Summary)

	for _, labelKey := range labelKeys {
		containers, err := GetLabeledContainers(ctx, apiClient, labelKey, stackName, true)
		if err != nil {
			return nil, err
		}

		for _, cont := range containers {
			containerByID[cont.ID] = cont
		}
	}

	using := make([]container.Summary, 0)

	for _, cont := range containerByID {
		for _, mount := range cont.Mounts {
			if mount.Name == volumeName {
				using = append(using, cont)
				break
			}
		}
	}

	return using, nil
}

func desiredVolumeConfigsByName(stackName string, project *types.Project) map[string]types.VolumeConfig {
	desiredVolumes := make(map[string]types.VolumeConfig)
	if project == nil {
		return desiredVolumes
	}

	for key, cfg := range project.Volumes {
		desiredVolumes[key] = cfg

		if cfg.Name != "" {
			desiredVolumes[cfg.Name] = cfg
		}

		if stackName != "" {
			desiredVolumes[stackName+"_"+key] = cfg

			if cfg.Name != "" {
				desiredVolumes[stackName+"_"+cfg.Name] = cfg
			}
		}
	}

	return desiredVolumes
}

// removeMismatchedRecreatableVolumes removes volumes that have mismatched configuration.
// This is intentionally limited to NFS/CIFS/tmpfs-backed volumes.
// Removing them allows Docker Compose to recreate them during service.Create.
func removeMismatchedRecreatableVolumes(ctx context.Context, apiClient client.APIClient, stackName string, project *types.Project) error {
	if len(project.Volumes) == 0 {
		return nil
	}

	// Get existing volumes for this stack across compose and swarm labels.
	labelKeys := []string{api.ProjectLabel, swarm.StackNamespaceLabel}
	existingVolumesByName := make(map[string]types.VolumeConfig)

	for _, labelKey := range labelKeys {
		vols, err := GetLabeledVolumes(ctx, apiClient, labelKey, stackName)
		if err != nil {
			return fmt.Errorf("failed to get existing volumes: %w", err)
		}

		for _, vol := range vols {
			existingVolumesByName[vol.Name] = types.VolumeConfig{
				Driver:     vol.Driver,
				DriverOpts: vol.Options,
			}
		}
	}

	if len(existingVolumesByName) == 0 {
		return nil
	}

	// Build a map of desired volume names including stack-name-prefixed variants used by swarm.
	desiredVolumes := desiredVolumeConfigsByName(stackName, project)

	// Check for mismatched volumes and remove them
	for existingName, existingCfg := range existingVolumesByName {
		desired, exists := desiredVolumes[existingName]
		if !exists {
			continue // Volume not in new config, will be handled by RemoveOrphans
		}

		existingIsRecreatable := isRecreatableVolumeType(&existingCfg)
		desiredIsRecreatable := isRecreatableVolumeType(&desired)

		if !existingIsRecreatable && !desiredIsRecreatable {
			continue
		}

		// If the existing volume config doesn't match the desired one, remove it
		if !volumeConfigMatch(&existingCfg, &desired) {
			containersUsingVolume, err := getContainersUsingVolume(ctx, apiClient, stackName, existingName)
			if err != nil {
				return fmt.Errorf("failed to get containers using volume %s: %w", existingName, err)
			}

			for _, cont := range containersUsingVolume {
				if !strings.EqualFold(string(cont.State), "running") {
					continue
				}

				slog.Debug("stopping running container using volume before recreate",
					slog.String("volume", existingName),
					slog.String("container_id", cont.ID),
					slog.Any("container_names", cont.Names),
				)

				_, err = apiClient.ContainerStop(ctx, cont.ID, client.ContainerStopOptions{})
				if err != nil {
					return fmt.Errorf("failed to stop container %s using volume %s: %w", cont.ID, existingName, err)
				}
			}

			for _, cont := range containersUsingVolume {
				slog.Debug("removing container using volume before recreate",
					slog.String("volume", existingName),
					slog.String("container_id", cont.ID),
					slog.Any("container_names", cont.Names),
				)

				_, err = apiClient.ContainerRemove(ctx, cont.ID, client.ContainerRemoveOptions{Force: true})
				if err != nil {
					return fmt.Errorf("failed to remove container %s using volume %s: %w", cont.ID, existingName, err)
				}
			}

			slog.Debug("removing volume with mismatched config", slog.String("volume", existingName),
				slog.String("existing_driver", existingCfg.Driver),
				slog.Any("existing_opts", existingCfg.DriverOpts),
				slog.String("desired_driver", desired.Driver),
				slog.Any("desired_opts", desired.DriverOpts),
			)

			retries := 3
			removed := false

			for range retries {
				_, err = apiClient.VolumeRemove(ctx, existingName, client.VolumeRemoveOptions{Force: true})
				if err != nil {
					if strings.Contains(err.Error(), ErrIsInUse.Error()) {
						time.Sleep(1 * time.Second)
						continue
					}

					return fmt.Errorf("failed to remove volume %s: %w", existingName, err)
				}

				removed = true

				break
			}

			if !removed {
				return fmt.Errorf("failed to remove volume %s: %w", existingName, err)
			}
		}
	}

	return nil
}
