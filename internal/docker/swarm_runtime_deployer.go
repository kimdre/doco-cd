package docker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/moby/moby/client"
)

// deploySwarmRuntime deploys a swarm stack using the provided request parameters.
func deploySwarmRuntime(ctx context.Context, req runtimeDeployRequest) error {
	deployConfig := req.request.DeployConfig
	configRetention := deployConfig.ResolveSwarmConfigRetention(req.request.SwarmRetention.Config)
	secretRetention := deployConfig.ResolveSwarmSecretRetention(req.request.SwarmRetention.Secret)

	req.phase.Set("deploying swarm stack")
	req.stackLog.Info("deploying swarm stack")

	cfg, opts, err := LoadSwarmStack(req.request.DockerCLI, req.project, deployConfig, req.externalWorkingDir)
	if err != nil {
		return fmt.Errorf("failed to load swarm stack: %w", err)
	}

	opts.Logger = req.stackLog
	opts.ForceUpdateServices = staleArtifactServices(req.request.DetectedChanges)

	addSwarmServiceLabels(cfg, req.project, deployConfig, req.request.Payload, req.request.SourceURL, req.externalWorkingDir,
		req.request.AppVersion, req.timestamp, req.request.LatestCommit, req.projectHash)
	addSwarmVolumeLabels(cfg, deployConfig, req.request.Payload)
	addSwarmConfigLabels(cfg, deployConfig, req.request.Payload, req.request.SourceURL, req.externalWorkingDir,
		req.request.AppVersion, req.timestamp, req.request.LatestCommit)
	addSwarmSecretLabels(cfg, deployConfig, req.request.Payload, req.request.SourceURL, req.externalWorkingDir,
		req.request.AppVersion, req.timestamp, req.request.LatestCommit)

	// Pinning is an optimization: if it fails, services are simply updated to the new artifact.
	if err = pinUnchangedSwarmServices(ctx, req.request.DockerCLI.Client(), cfg, deployConfig.Name, req.request.ExternalRepoPath, req.stackLog); err != nil {
		req.stackLog.Warn("failed to keep unchanged services on their current artifact", slog.Any("error", err))
	}

	var imagesBefore map[string]string

	if deployConfig.PruneImages {
		imagesBefore, err = getDeployedServiceImageRefs(ctx, req.request.DockerCLI, true, deployConfig.Name, req.stackLog)
		if err != nil {
			req.recordError()

			return fmt.Errorf("failed to get service images before deployment: %w", err)
		}
	}

	if err = removeMismatchedRecreatableVolumes(ctx, req.request.DockerCLI.Client(), deployConfig.Name, req.project); err != nil {
		req.recordError()

		return fmt.Errorf("failed to remove mismatched recreatable volumes: %w", err)
	}

	if err = DeploySwarmStack(ctx, req.request.DockerCLI, cfg, opts); err != nil {
		req.recordError()

		return fmt.Errorf("failed to deploy swarm stack %s: %w", deployConfig.Name, err)
	}

	if configRetention >= 0 {
		req.phase.Set("pruning stack configs")

		if err = PruneStackConfigs(ctx, req.request.DockerCLI.Client(), deployConfig.Name, configRetention); err != nil {
			req.recordError()

			return fmt.Errorf("failed to prune stack configs: %w", err)
		}
	} else {
		req.stackLog.Info("skipping swarm config prune: retention disabled", slog.Int("retention", configRetention))
	}

	if secretRetention >= 0 {
		req.phase.Set("pruning stack secrets")

		if err = PruneStackSecrets(ctx, req.request.DockerCLI.Client(), deployConfig.Name, secretRetention); err != nil {
			req.recordError()

			return fmt.Errorf("failed to prune stack secrets: %w", err)
		}
	} else {
		req.stackLog.Info("skipping swarm secret prune: retention disabled", slog.Int("retention", secretRetention))
	}

	if deployConfig.PruneImages {
		if err = removeReplacedSwarmImages(ctx, req, imagesBefore); err != nil {
			req.recordError()

			return err
		}
	}

	return nil
}

// removeReplacedSwarmImages removes the images the deployment replaced from every node.
func removeReplacedSwarmImages(ctx context.Context, req runtimeDeployRequest, imagesBefore map[string]string) error {
	imagesInUse, err := clusterServiceImageRefs(ctx, req.request.DockerCLI.Client())
	if err != nil {
		return fmt.Errorf("failed to get service images after deployment: %w", err)
	}

	replaced := replacedImageRefs(imagesBefore, imagesInUse)
	if len(replaced) == 0 {
		return nil
	}

	req.phase.Set("removing replaced images on swarm nodes")
	req.stackLog.Info("removing replaced images on swarm nodes", slog.Any("images", replaced))

	if err = RunImageRemoveJob(ctx, req.request.DockerCLI, replaced, req.stackLog); err != nil {
		return fmt.Errorf("failed to run image remove job: %w", err)
	}

	return nil
}

// clusterServiceImageRefs returns the image reference of every service in the cluster,
// so an image another stack still uses is not treated as replaced.
func clusterServiceImageRefs(ctx context.Context, apiClient client.APIClient) (map[string]string, error) {
	result, err := apiClient.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list swarm services: %w", err)
	}

	refs := make(map[string]string, len(result.Items))

	for _, svc := range result.Items {
		if svc.Spec.TaskTemplate.ContainerSpec != nil {
			refs[svc.ID] = svc.Spec.TaskTemplate.ContainerSpec.Image
		}
	}

	return refs, nil
}

// replacedImageRefs returns the refs in before whose image no ref in after uses,
// comparing by digest when present so a new tag for the same image is not a replacement.
func replacedImageRefs(before, after map[string]string) []string {
	inUse := make(map[string]struct{}, len(after))
	for _, ref := range after {
		inUse[imageIdentity(ref)] = struct{}{}
	}

	var replaced []string

	for _, ref := range before {
		if _, ok := inUse[imageIdentity(ref)]; !ok {
			replaced = append(replaced, ref)
		}
	}

	slices.Sort(replaced)

	return slices.Compact(replaced)
}

func imageIdentity(ref string) string {
	if digest := digestFromReference(ref); digest != "" {
		return digest
	}

	return ref
}
