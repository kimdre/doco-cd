package docker

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// composeDeployOptions carries the optional inputs of deployCompose.
type composeDeployOptions struct {
	// ArtifactRoot is the host path of the immutable artifact directory the project was loaded
	// from ("<store>/artifacts/<revision>"). When empty, services are not pinned to the artifact
	// they are currently running from.
	ArtifactRoot string
	// Log receives debug information about the deployment. Optional.
	Log *slog.Logger
}

func (o composeDeployOptions) logger() *slog.Logger {
	if o.Log == nil {
		return slog.New(slog.DiscardHandler)
	}

	return o.Log
}

// artifactRootFromWorkingDir returns the artifact root of a deployment from its working
// directory label and the working directory configured in its deploy config, or an empty
// string if the working directory is not part of an artifact.
func artifactRootFromWorkingDir(workingDir, configuredWorkingDir string) string {
	workingDir = strings.TrimSpace(workingDir)
	if workingDir == "" {
		return ""
	}

	root := filepath.Clean(workingDir)

	rel := strings.TrimPrefix(filepath.Clean(string(filepath.Separator)+configuredWorkingDir), string(filepath.Separator))
	if rel != "" {
		trimmed, ok := strings.CutSuffix(root, string(filepath.Separator)+rel)
		if !ok {
			return ""
		}

		root = trimmed
	}

	if filepath.Base(filepath.Dir(root)) != store.ArtifactsSubdir {
		return ""
	}

	return root
}

// pinUnchangedComposeServices keeps services on the artifact their containers were created from
// when none of the repository files they use changed since.
//
// Every revision is deployed from its own immutable artifact directory, so all absolute paths
// into the repository (bind-mount sources, env and label files, watch paths) change with every
// revision. These paths are part of the Compose service config hash, which would otherwise make
// Compose recreate every service using them on each deployment, even if the content of all
// files they use is unchanged, see https://github.com/kimdre/doco-cd/issues/1911.
//
// A service is only pinned when all of its containers were created from the same, still existing
// artifact of the same store, their bind mounts match that artifact, and the content of every
// repository path the service uses is identical in both artifacts. The paths of a pinned service
// are rewritten to the artifact of its containers and the service is labeled with that artifact's
// revision (see DocoCDLabels.Deployment.PinnedRevisions), which keeps it from being removed by the
// artifact garbage collection. All other metadata (working directory, compose files, commit)
// still references the deployed revision.
//
// Services that are force-recreated are never pinned, since they are recreated anyway.
func pinUnchangedComposeServices(
	ctx context.Context,
	apiClient client.APIClient,
	project *types.Project,
	artifactRoot string,
	recreateMode string,
	forcedServices []string,
	log *slog.Logger,
) error {
	if artifactRoot == "" || (recreateMode == api.RecreateForce && len(forcedServices) == 0) {
		return nil
	}

	artifactRoot = filepath.Clean(artifactRoot)

	artifactsDir := filepath.Dir(artifactRoot)
	if filepath.Base(artifactsDir) != store.ArtifactsSubdir || !filesystem.IsDir(artifactRoot) {
		return nil
	}

	storeBase := filepath.Dir(artifactsDir)

	containersByService, err := composeServiceContainers(ctx, apiClient, project.Name)
	if err != nil {
		return fmt.Errorf("list containers of project %s: %w", project.Name, err)
	}

	for name, service := range project.Services {
		if recreateMode == api.RecreateForce && slices.Contains(forcedServices, name) {
			continue
		}

		containers := containersByService[name]
		if len(containers) == 0 {
			continue
		}

		serviceLog := log.With(slog.String("service", name))

		creationRoot, revision, ok := containersArtifactRoot(containers, storeBase)
		if !ok || creationRoot == artifactRoot {
			continue
		}

		if !filesystem.IsDir(creationRoot) {
			serviceLog.Debug("artifact of existing containers no longer exists, not pinning service",
				slog.String("artifact", creationRoot))

			continue
		}

		pinned, ok, err := pinComposeService(service, containers, artifactRoot, creationRoot)
		if err != nil {
			serviceLog.Debug("failed to compare repository files of service, not pinning service", slog.Any("error", err))

			continue
		}

		if !ok {
			continue
		}

		pinned.CustomLabels = maps.Clone(pinned.CustomLabels)
		if pinned.CustomLabels == nil {
			pinned.CustomLabels = types.Labels{}
		}

		pinned.CustomLabels[DocoCDLabels.Deployment.PinnedRevisions] = string(revision)
		project.Services[name] = pinned

		serviceLog.Debug("repository files of service are unchanged, keeping service on its current artifact",
			slog.String("artifact", creationRoot))
	}

	return nil
}

// composeServiceContainers returns the containers of a project by service, excluding one-off
// and ephemeral job containers, which Compose does not reconcile.
func composeServiceContainers(ctx context.Context, apiClient client.APIClient, projectName string) (map[string][]container.Summary, error) {
	result, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
		All: true,
		Filters: make(client.Filters).
			Add("label", api.ProjectLabel+"="+projectName).
			Add("label", api.OneoffLabel+"=False"),
	})
	if err != nil {
		return nil, err
	}

	byService := make(map[string][]container.Summary)

	for _, c := range result.Items {
		if strings.EqualFold(strings.TrimSpace(c.Labels[DocoCDJobLabels.JobEphemeral]), "true") {
			continue
		}

		if service := c.Labels[api.ServiceLabel]; service != "" {
			byService[service] = append(byService[service], c)
		}
	}

	return byService, nil
}

// containersArtifactRoot returns the artifact all containers were created from.
func containersArtifactRoot(containers []container.Summary, storeBase string) (string, store.Revision, bool) {
	var (
		root     string
		revision store.Revision
	)

	for i, c := range containers {
		containerRoot, containerRevision, ok := containerArtifactRoot(c.Labels, storeBase)
		if !ok || (i > 0 && containerRoot != root) {
			return "", "", false
		}

		root, revision = containerRoot, containerRevision
	}

	return root, revision, root != ""
}

// containerArtifactRoot returns the artifact a container was created from: the artifact it was
// pinned to, or the artifact of the revision it was deployed with.
func containerArtifactRoot(labels map[string]string, storeBase string) (string, store.Revision, bool) {
	if pinned := ParsePinnedRevisions(labels[DocoCDLabels.Deployment.PinnedRevisions]); len(pinned) > 0 {
		if len(pinned) != 1 {
			return "", "", false
		}

		revision := store.Revision(pinned[0])

		return filepath.Join(storeBase, store.ArtifactsSubdir, store.ArtifactDirName(revision)), revision, true
	}

	workingDir := strings.TrimSpace(labels[DocoCDLabels.Deployment.WorkingDir])
	if workingDir == "" {
		return "", "", false
	}

	return store.ArtifactRoot(storeBase, filepath.Clean(workingDir))
}

// rebasePath returns path relative to newRoot joined to oldRoot.
func rebasePath(path, newRoot, oldRoot string) (string, bool) {
	if path == "" || !filepath.IsAbs(path) {
		return "", false
	}

	rel, err := filepath.Rel(newRoot, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}

	return filepath.Join(oldRoot, rel), true
}

// pinComposeService returns service with every path into newRoot rewritten to oldRoot, if the
// content of all of these paths is identical in both roots and the bind mounts of all containers
// match oldRoot. It reports false if the service does not use any path into newRoot.
func pinComposeService(service types.ServiceConfig, containers []container.Summary, newRoot, oldRoot string) (types.ServiceConfig, bool, error) {
	type pathPair struct{ current, pinned string }

	var pairs []pathPair

	rebase := func(path string) (string, bool) {
		pinned, ok := rebasePath(path, newRoot, oldRoot)
		if ok {
			pairs = append(pairs, pathPair{current: path, pinned: pinned})
		}

		return pinned, ok
	}

	volumes := slices.Clone(service.Volumes)
	for i, volume := range volumes {
		if volume.Type != types.VolumeTypeBind {
			continue
		}

		pinned, ok := rebase(volume.Source)
		if !ok {
			continue
		}

		for _, c := range containers {
			if !containerHasBindMount(c, pinned, volume.Target) {
				return service, false, nil
			}
		}

		volumes[i].Source = pinned
	}

	envFiles := slices.Clone(service.EnvFiles)
	for i, envFile := range envFiles {
		if pinned, ok := rebase(envFile.Path); ok {
			envFiles[i].Path = pinned
		}
	}

	labelFiles := slices.Clone(service.LabelFiles)
	for i, labelFile := range labelFiles {
		if pinned, ok := rebase(labelFile); ok {
			labelFiles[i] = pinned
		}
	}

	var develop *types.DevelopConfig

	if service.Develop != nil {
		developCopy := *service.Develop
		developCopy.Watch = slices.Clone(service.Develop.Watch)

		for i, trigger := range developCopy.Watch {
			if pinned, ok := rebase(trigger.Path); ok {
				developCopy.Watch[i].Path = pinned
			}
		}

		develop = &developCopy
	}

	if len(pairs) == 0 {
		return service, false, nil
	}

	for _, pair := range pairs {
		equal, err := filesystem.ContentEqual(pair.current, pair.pinned)
		if err != nil {
			return service, false, err
		}

		if !equal {
			return service, false, nil
		}
	}

	service.Volumes = volumes
	service.EnvFiles = envFiles
	service.LabelFiles = labelFiles
	service.Develop = develop

	return service, true, nil
}

// containerHasBindMount reports whether c bind-mounts source at target.
func containerHasBindMount(c container.Summary, source, target string) bool {
	for _, m := range c.Mounts {
		if m.Type == mount.TypeBind && filepath.Clean(m.Destination) == filepath.Clean(target) {
			return filepath.Clean(m.Source) == filepath.Clean(source)
		}
	}

	return false
}
