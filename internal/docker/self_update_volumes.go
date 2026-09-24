package docker

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/errdefs"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// validateSelfUpdateVolumes refuses changes to existing volumes because
// rollback cannot restore their data after any Compose Create.
func validateSelfUpdateVolumes(ctx context.Context, apiClient client.APIClient, project *types.Project) error {
	if len(project.Volumes) == 0 {
		return nil
	}

	list, err := apiClient.VolumeList(ctx, client.VolumeListOptions{
		Filters: make(client.Filters).Add("label", api.ProjectLabel+"="+project.Name),
	})
	if err != nil {
		return fmt.Errorf("list self stack volumes before update: %w", err)
	}

	for _, key := range slices.Sorted(maps.Keys(project.Volumes)) {
		desired := project.Volumes[key]
		if desired.External {
			continue
		}

		if desired.Name == "" {
			desired.Name = project.Name + "_" + key
		}

		hash, hashErr := compose.VolumeHash(desired)
		if hashErr != nil {
			return fmt.Errorf("hash self stack volume %s: %w", desired.Name, hashErr)
		}

		for _, existing := range list.Items {
			if existing.Labels[api.VolumeLabel] != key {
				continue
			}

			if existing.Name != desired.Name {
				return unsafeSelfUpdateVolume(existing.Name, key, "the desired volume name changed")
			}

			if err := checkSelfUpdateVolume(existing, desired, hash, key); err != nil {
				return err
			}
		}

		inspected, inspectErr := apiClient.VolumeInspect(ctx, desired.Name, client.VolumeInspectOptions{})
		if errdefs.IsNotFound(inspectErr) {
			continue
		}

		if inspectErr != nil {
			return fmt.Errorf("inspect self stack volume %s: %w", desired.Name, inspectErr)
		}

		if inspected.Volume.Labels[api.ProjectLabel] == project.Name {
			if actualKey := inspected.Volume.Labels[api.VolumeLabel]; actualKey != "" && actualKey != key {
				return unsafeSelfUpdateVolume(desired.Name, key, "another project volume owns this name")
			}

			if err := checkSelfUpdateVolume(inspected.Volume, desired, hash, key); err != nil {
				return err
			}
		}
	}

	return nil
}

// checkSelfUpdateVolume compares the live volume with its requested settings.
func checkSelfUpdateVolume(existing volume.Volume, desired types.VolumeConfig, hash, key string) error {
	if liveHash := existing.Labels[api.ConfigHashLabel]; liveHash != "" && liveHash != hash {
		return unsafeSelfUpdateVolume(existing.Name, key, "the Compose configuration hash changed")
	}

	live := types.VolumeConfig{Driver: existing.Driver, DriverOpts: existing.Options, Labels: existing.Labels}
	if !volumeConfigMatch(&live, &desired) {
		return unsafeSelfUpdateVolume(existing.Name, key, "its driver, options, or labels changed")
	}

	return nil
}

// unsafeSelfUpdateVolume reports why a volume change cannot be rolled back.
func unsafeSelfUpdateVolume(name, key, reason string) error {
	return fmt.Errorf("%w: cannot self-update with existing volume %q (%s): %s; volume data cannot be restored",
		selfupdate.ErrUnsupported, name, key, reason)
}
