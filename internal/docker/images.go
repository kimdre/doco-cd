package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/utils/set"
)

// registryAuthForImage returns a base64-encoded auth string for the registry
// that hosts the given image ref, sourced from the Docker config file.
// Returns an empty string on any error (unauthenticated access is attempted).
func registryAuthForImage(dockerCli command.Cli, imageRef string) string {
	encoded, err := command.RetrieveAuthTokenFromImage(dockerCli.ConfigFile(), imageRef)
	if err != nil {
		return ""
	}

	return encoded
}

// registryDigestForRef queries the registry for the current manifest digest of
// the given image reference without downloading any image layers.
func registryDigestForRef(ctx context.Context, dockerCli command.Cli, imageRef string) (string, error) {
	info, err := dockerCli.Client().DistributionInspect(ctx, imageRef, client.DistributionInspectOptions{
		EncodedRegistryAuth: registryAuthForImage(dockerCli, imageRef),
	})
	if err != nil {
		return "", fmt.Errorf("registry inspect failed for %s: %w", imageRef, err)
	}

	return info.Descriptor.Digest.String(), nil
}

// digestFromReference extracts the digest part from an image reference in
// "name@digest" form and returns an empty string when no digest is present.
func digestFromReference(ref string) string {
	_, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return ""
	}

	return digest
}

// digestFromRepoDigests returns the first digest found in RepoDigests.
// It returns an empty string when no digest entry can be parsed.
func digestFromRepoDigests(repoDigests []string) string {
	for _, repoDigest := range repoDigests {
		digest := digestFromReference(repoDigest)
		if digest != "" {
			return digest
		}
	}

	return ""
}

// getDeployedServiceImageDigests collects deployed service digests keyed by
// service name for the given project in both Swarm and non-Swarm modes.
func getDeployedServiceImageDigests(ctx context.Context, dockerCli command.Cli, projectName string, logger *slog.Logger) (map[string]string, error) {
	deployed := make(map[string]string)

	if swarmInternal.GetModeEnabled() {
		services, err := swarmInternal.GetStackServices(ctx, dockerCli.Client(), projectName)
		if err != nil {
			return nil, fmt.Errorf("failed to list swarm services for %s: %w", projectName, err)
		}

		ns := convert.NewNamespace(projectName)
		for _, svc := range services {
			svcName := ns.Descope(svc.Spec.Name)

			digest := digestFromReference(svc.Spec.TaskTemplate.ContainerSpec.Image)
			if digest == "" {
				logger.Warn("deployed swarm service image has no digest", slog.String("service", svcName), slog.String("image", svc.Spec.TaskTemplate.ContainerSpec.Image))
				continue
			}

			deployed[svcName] = digest
		}

		return deployed, nil
	}

	containers, err := GetProjectContainers(ctx, dockerCli, projectName)
	if err != nil {
		return nil, fmt.Errorf("failed to list project containers for %s: %w", projectName, err)
	}

	selected := make(map[string]api.ContainerSummary)

	for _, cont := range containers {
		svcName := cont.Labels[api.ServiceLabel]
		if svcName == "" {
			continue
		}

		existing, ok := selected[svcName]
		if !ok || (existing.State != container.StateRunning && cont.State == container.StateRunning) {
			selected[svcName] = cont
		}
	}

	for svcName, cont := range selected {
		contInspect, err := dockerCli.Client().ContainerInspect(ctx, cont.ID, client.ContainerInspectOptions{})
		if err != nil {
			logger.Warn("failed to inspect deployed container", slog.String("service", svcName), slog.String("container_id", cont.ID), slog.String("err", err.Error()))
			continue
		}

		imageID := contInspect.Container.Image

		img, err := dockerCli.Client().ImageInspect(ctx, imageID)
		if err != nil {
			logger.Warn("failed to inspect deployed image", slog.String("service", svcName), slog.String("image_id", imageID), slog.String("err", err.Error()))
			continue
		}

		digest := digestFromRepoDigests(img.RepoDigests)
		if digest == "" {
			logger.Warn("deployed image has no digest", slog.String("service", svcName), slog.String("image_id", imageID))
			continue
		}

		deployed[svcName] = digest
	}

	return deployed, nil
}

// pruneImages tries to remove the specified image IDs from the Docker host and
// returns a list of pruned image IDs. Images still in use are silently skipped.
func pruneImages(ctx context.Context, dockerCli command.Cli, images []string) ([]string, error) {
	var prunedImages []string

	for _, img := range images {
		result, err := dockerCli.Client().ImageRemove(ctx, img, client.ImageRemoveOptions{
			Force:         true,
			PruneChildren: true,
		})
		if err != nil {
			switch {
			case strings.Contains(err.Error(), "image is being used by running container"):
				continue
			case strings.Contains(strings.ToLower(err.Error()), "no such image"),
				strings.Contains(strings.ToLower(err.Error()), "not found"):
				continue
			default:
				return nil, fmt.Errorf("failed to remove image %s: %w", img, err)
			}
		}

		for _, r := range result.Items {
			switch {
			case r.Deleted != "":
				prunedImages = append(prunedImages, r.Deleted)
			case r.Untagged != "":
				prunedImages = append(prunedImages, r.Untagged)
			}
		}
	}

	return prunedImages, nil
}

// PullImages pulls all images defined in the named compose project.
func PullImages(ctx context.Context, dockerCli command.Cli, projectName string) error {
	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	containers, err := GetProjectContainers(ctx, dockerCli, projectName)
	if err != nil {
		return fmt.Errorf("failed to get project containers: %w", err)
	}

	containerNames := make([]string, 0, len(containers))
	for _, c := range containers {
		containerNames = append(containerNames, c.Name)
	}

	project, err := service.Generate(ctx, api.GenerateOptions{ProjectName: projectName, Containers: containerNames})
	if err != nil {
		return fmt.Errorf("failed to generate project: %w", err)
	}

	return service.Pull(ctx, project, api.PullOptions{Quiet: true})
}

// vars used to allow overriding in tests without needing to mock the entire function
var (
	registryDigestLookup        = registryDigestForRef           // registryDigestLookup fetches registry digests for image refs, can be overridden in tests
	deployedServiceDigestLookup = getDeployedServiceImageDigests // deployedServiceDigestLookup fetches deployed service image digests, can be overridden in tests
)

// HaveDeployedServiceImageDigestsChanged checks if any currently deployed
// service image digest differs from the registry digest of the configured image ref.
//
// This compares:
//  1. deployed service image digest (currently running/deployed)
//  2. registry digest of configured service image reference (DistributionInspect)
//
// Returns true as soon as one service differs.
func HaveDeployedServiceImageDigestsChanged(ctx context.Context, dockerCli command.Cli, project *types.Project, logger *slog.Logger) (bool, error) {
	// service name -> configured image ref
	configuredRefs := make(map[string]string)
	uniqueRefs := set.New[string]()

	for _, svc := range project.Services {
		if svc.Image == "" {
			continue
		}

		configuredRefs[svc.Name] = svc.Image
		uniqueRefs.Add(svc.Image)
	}

	if len(configuredRefs) == 0 {
		return false, nil
	}

	registryDigests := make(map[string]string, uniqueRefs.Len())
	for _, ref := range uniqueRefs.ToSlice() {
		digest, err := registryDigestLookup(ctx, dockerCli, ref)
		if err != nil {
			logger.Warn("could not fetch registry digest", slog.String("ref", ref), slog.String("err", err.Error()))
			continue
		}

		registryDigests[ref] = digest
	}

	deployedDigests, err := deployedServiceDigestLookup(ctx, dockerCli, project.Name, logger)
	if err != nil {
		return false, err
	}

	for serviceName, configuredRef := range configuredRefs {
		registryDigest, ok := registryDigests[configuredRef]
		if !ok {
			logger.Warn("registry digest unavailable, skipping service", slog.String("service", serviceName), slog.String("ref", configuredRef))
			continue
		}

		deployedDigest, ok := deployedDigests[serviceName]
		if !ok {
			logger.Info("deployed service digest unavailable, treating as changed", slog.String("service", serviceName), slog.String("ref", configuredRef))
			return true, nil
		}

		if deployedDigest != registryDigest {
			logger.Info("service image digest changed",
				slog.String("service", serviceName),
				slog.String("ref", configuredRef),
				slog.String("deployed", deployedDigest),
				slog.String("registry", registryDigest),
			)

			return true, nil
		}
	}

	return false, nil
}
