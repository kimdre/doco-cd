package docker

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/cli/cli/compose/convert"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// pinUnchangedSwarmServices keeps the bind mounts of Swarm services on the artifact they are
// currently mounted from when none of the mounted repository files changed since.
//
// Every revision is deployed from its own immutable artifact directory, so the source of every
// bind mount into the repository changes with every revision. Bind-mount sources are part of the
// task template, which would otherwise make Swarm recreate the tasks of every service with such a
// bind mount on each deployment, even if the content of the mounted files is unchanged.
//
// A service is only pinned when the currently deployed service mounts every repository path at
// the same target from a still existing artifact of the same store and the content of every
// mounted path is identical in both artifacts. The bind-mount sources of a pinned service are
// rewritten to the currently mounted artifacts and the service is labeled with their revisions
// (see DocoCDLabels.Deployment.PinnedRevisions), which keeps them from being removed by the
// artifact garbage collection.
func pinUnchangedSwarmServices(
	ctx context.Context,
	apiClient client.APIClient,
	stack *composetypes.Config,
	namespace, artifactRoot string,
	log *slog.Logger,
) error {
	if artifactRoot == "" {
		return nil
	}

	artifactRoot = filepath.Clean(artifactRoot)

	artifactsDir := filepath.Dir(artifactRoot)
	if filepath.Base(artifactsDir) != store.ArtifactsSubdir || !filesystem.IsDir(artifactRoot) {
		return nil
	}

	storeBase := filepath.Dir(artifactsDir)

	services, err := swarmInternal.GetStackServices(ctx, apiClient, namespace)
	if err != nil {
		return fmt.Errorf("list services of stack %s: %w", namespace, err)
	}

	existing := make(map[string]swarm.Service, len(services))
	for _, service := range services {
		existing[service.Spec.Name] = service
	}

	scope := convert.NewNamespace(namespace)

	for i, service := range stack.Services {
		current, ok := existing[scope.Scope(service.Name)]
		if !ok || current.Spec.TaskTemplate.ContainerSpec == nil {
			continue
		}

		serviceLog := log.With(slog.String("service", service.Name))

		volumes, revisions, ok, err := pinSwarmServiceMounts(service.Volumes, current.Spec.TaskTemplate.ContainerSpec.Mounts, storeBase, artifactRoot)
		if err != nil {
			serviceLog.Debug("failed to compare bind-mounted repository files of service, not pinning service", slog.Any("error", err))

			continue
		}

		if !ok {
			continue
		}

		service.Volumes = volumes

		if service.Deploy.Labels == nil {
			service.Deploy.Labels = make(map[string]string)
		}

		service.Deploy.Labels[DocoCDLabels.Deployment.PinnedRevisions] = FormatPinnedRevisions(revisions)
		stack.Services[i] = service

		serviceLog.Debug("bind-mounted repository files of service are unchanged, keeping service on its current artifacts",
			slog.Any("revisions", revisions))
	}

	return nil
}

// pinSwarmServiceMounts returns volumes with the source of every bind mount into newRoot replaced
// by the source currently mounted at the same target, if it is the same path in another existing
// artifact of storeBase with identical content. It reports false if any bind mount into newRoot
// cannot be pinned or if there is none.
func pinSwarmServiceMounts(volumes []composetypes.ServiceVolumeConfig, currentMounts []mount.Mount, storeBase, newRoot string) (
	[]composetypes.ServiceVolumeConfig, []string, bool, error,
) {
	var revisions []string

	pinned := slices.Clone(volumes)

	for i, volume := range pinned {
		if volume.Type != string(mount.TypeBind) || !filepath.IsAbs(volume.Source) {
			continue
		}

		rel, err := filepath.Rel(newRoot, filepath.Clean(volume.Source))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}

		source, ok := currentBindSource(currentMounts, volume.Target)
		if !ok {
			return nil, nil, false, nil
		}

		oldRoot, revision, ok := store.ArtifactRoot(storeBase, source)
		if !ok || oldRoot == newRoot || filepath.Join(oldRoot, rel) != source || !filesystem.IsDir(oldRoot) {
			return nil, nil, false, nil
		}

		equal, err := artifactContentEqual(newRoot, volume.Source, oldRoot, source)
		if err != nil || !equal {
			return nil, nil, false, err
		}

		pinned[i].Source = source

		if !slices.Contains(revisions, string(revision)) {
			revisions = append(revisions, string(revision))
		}
	}

	if len(revisions) == 0 {
		return nil, nil, false, nil
	}

	slices.Sort(revisions)

	return pinned, revisions, true, nil
}

// currentBindSource returns the source of the bind mount at target.
func currentBindSource(mounts []mount.Mount, target string) (string, bool) {
	for _, m := range mounts {
		if filepath.Clean(m.Target) == filepath.Clean(target) {
			if m.Type != mount.TypeBind {
				return "", false
			}

			return filepath.Clean(m.Source), true
		}
	}

	return "", false
}
