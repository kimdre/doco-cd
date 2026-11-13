package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/containerd/errdefs"
	distreference "github.com/distribution/reference"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
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

const (
	imageRemoveTaskWait     = time.Minute
	imageRemoveTaskPoll     = time.Second
	deferredImageRemoveName = app.Name + "_image-remove-deferred-"
	// deferredImageRemoveLabel holds the ref of an image a run could not remove yet. The configs
	// carrying it are cluster-wide, so any later run, from any doco-cd instance, retries them.
	deferredImageRemoveLabel = "cd.doco.image-remove.deferred"
)

// removeReplacedSwarmImages removes the images the deployment replaced from every node, plus
// those earlier runs deferred. An image whose old tasks are still stopping is deferred again.
func removeReplacedSwarmImages(ctx context.Context, req runtimeDeployRequest, imagesBefore map[string]string) error {
	return removeReplacedSwarmImagesWithin(ctx, req, imagesBefore, imageRemoveTaskWait)
}

func removeReplacedSwarmImagesWithin(ctx context.Context, req runtimeDeployRequest, imagesBefore map[string]string, taskWait time.Duration) error {
	apiClient := req.request.DockerCLI.Client()

	imagesInUse, err := clusterServiceImageRefs(ctx, apiClient)
	if err != nil {
		return fmt.Errorf("failed to get service images after deployment: %w", err)
	}

	deferred, err := deferredImageRemovals(ctx, apiClient)
	if err != nil {
		return fmt.Errorf("failed to list deferred image removals: %w", err)
	}

	candidates := maps.Clone(imagesBefore)
	if candidates == nil {
		candidates = make(map[string]string, len(deferred))
	}

	for ref, configID := range deferred {
		candidates["deferred:"+configID] = ref
	}

	if inUse, _, ok := serviceImageUsage(imagesInUse); ok {
		forgetDeferredImageRemovals(ctx, apiClient, deferred, func(ref string) bool {
			_, digest, parsed := parseImageRef(ref)
			_, used := inUse[digest]

			return parsed && used
		}, req.stackLog)
	}

	replaced := replacedImageRefs(candidates, imagesInUse)
	if len(replaced) == 0 {
		return nil
	}

	stopping, err := waitForImageTasksStopped(ctx, apiClient, replaced, taskWait)
	if err != nil {
		return fmt.Errorf("failed to wait for tasks of replaced images: %w", err)
	}

	if len(stopping) > 0 {
		req.stackLog.Warn("deferring removal of images whose tasks are still stopping", slog.Any("images", stopping))

		if err = deferImageRemovals(ctx, apiClient, stopping, deferred); err != nil {
			return fmt.Errorf("failed to defer image removals: %w", err)
		}
	}

	ready := slices.DeleteFunc(replaced, func(ref string) bool { return slices.Contains(stopping, ref) })
	if len(ready) == 0 {
		return nil
	}

	req.phase.Set("removing replaced images on swarm nodes")
	req.stackLog.Info("removing replaced images on swarm nodes", slog.Any("images", ready))

	if err = RunImageRemoveJob(ctx, req.request.DockerCLI, ready, req.stackLog); err != nil {
		return fmt.Errorf("failed to run image remove job: %w", err)
	}

	forgetDeferredImageRemovals(ctx, apiClient, deferred, func(ref string) bool {
		return slices.Contains(ready, ref)
	}, req.stackLog)

	return nil
}

// waitForImageTasksStopped waits until no task anywhere in the cluster runs one of refs, since
// the daemon refuses to remove an image a running container uses. It returns the refs that
// still had a live task when timeout elapsed.
func waitForImageTasksStopped(ctx context.Context, apiClient client.APIClient, refs []string, timeout time.Duration) ([]string, error) {
	byDigest := make(map[string]string, len(refs))

	for _, ref := range refs {
		if _, digest, ok := parseImageRef(ref); ok && digest != "" {
			byDigest[digest] = ref
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(imageRemoveTaskPoll)
	defer ticker.Stop()

	for {
		tasks, err := apiClient.TaskList(ctx, client.TaskListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list swarm tasks: %w", err)
		}

		var live []string

		for _, task := range tasks.Items {
			if swarmTaskStopped(task.Status.State) || task.Spec.ContainerSpec == nil {
				continue
			}

			_, digest, ok := parseImageRef(task.Spec.ContainerSpec.Image)
			if ref, replaced := byDigest[digest]; ok && replaced {
				live = append(live, ref)
			}
		}

		slices.Sort(live)
		live = slices.Compact(live)

		if len(live) == 0 {
			return nil, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-waitCtx.Done():
			return live, nil
		case <-ticker.C:
		}
	}
}

// deferredImageRemovals returns the images earlier runs deferred, keyed by ref, with the ID of
// the config that records each.
func deferredImageRemovals(ctx context.Context, apiClient client.APIClient) (map[string]string, error) {
	result, err := apiClient.ConfigList(ctx, client.ConfigListOptions{
		Filters: make(client.Filters).Add("label", deferredImageRemoveLabel),
	})
	if err != nil {
		return nil, err
	}

	deferred := make(map[string]string, len(result.Items))

	for _, cfg := range result.Items {
		if ref := cfg.Spec.Labels[deferredImageRemoveLabel]; ref != "" {
			deferred[ref] = cfg.ID
		}
	}

	return deferred, nil
}

func deferImageRemovals(ctx context.Context, apiClient client.APIClient, refs []string, deferred map[string]string) error {
	for _, ref := range refs {
		if _, ok := deferred[ref]; ok {
			continue
		}

		sum := sha256.Sum256([]byte(ref))
		spec := swarmTypes.ConfigSpec{
			Annotations: swarmTypes.Annotations{
				Name: deferredImageRemoveName + hex.EncodeToString(sum[:])[:20],
				Labels: map[string]string{
					DocoCDLabels.Metadata.Manager: app.Name,
					deferredImageRemoveLabel:      ref,
				},
			},
			Data: []byte(ref),
		}

		// A concurrent run may have deferred the same image first.
		_, err := apiClient.ConfigCreate(ctx, client.ConfigCreateOptions{Spec: spec})
		if err != nil && !errdefs.IsConflict(err) && !errdefs.IsAlreadyExists(err) {
			return fmt.Errorf("record deferred removal of %s: %w", ref, err)
		}
	}

	return nil
}

// forgetDeferredImageRemovals removes the records of the deferred images matching done. A record
// that cannot be removed only means the image is retried by a later run.
func forgetDeferredImageRemovals(ctx context.Context, apiClient client.APIClient, deferred map[string]string, done func(ref string) bool, log *slog.Logger) {
	for ref, configID := range deferred {
		if !done(ref) {
			continue
		}

		_, err := apiClient.ConfigRemove(ctx, configID, client.ConfigRemoveOptions{})
		if err != nil && !errdefs.IsNotFound(err) {
			log.Warn("failed to remove deferred image removal record", slog.String("image", ref), slog.Any("error", err))
		}
	}
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

// replacedImageRefs returns a repo@digest ref for each image in before that no service in after uses.
// Only digest-pinned refs are candidates, since a tag can move before the removal runs, and a
// candidate is kept while a service references its repository without a digest.
func replacedImageRefs(before, after map[string]string) []string {
	inUse, unpinnedRepos, ok := serviceImageUsage(after)
	if !ok {
		return nil
	}

	var replaced []string

	for _, ref := range before {
		repo, digest, ok := parseImageRef(ref)
		if !ok || digest == "" {
			continue
		}

		_, used := inUse[digest]
		_, ambiguous := unpinnedRepos[repo]

		if !used && !ambiguous {
			replaced = append(replaced, repo+"@"+digest)
		}
	}

	slices.Sort(replaced)

	return slices.Compact(replaced)
}

// serviceImageUsage returns the digests the given service images pin and the repositories
// they reference without a digest. It reports false when a ref cannot be parsed, since an
// unknown image may be any of them.
func serviceImageUsage(images map[string]string) (digests, unpinnedRepos map[string]struct{}, ok bool) {
	digests = make(map[string]struct{}, len(images))
	unpinnedRepos = make(map[string]struct{})

	for _, ref := range images {
		repo, digest, parsed := parseImageRef(ref)
		if !parsed {
			return nil, nil, false
		}

		if digest == "" {
			unpinnedRepos[repo] = struct{}{}
		} else {
			digests[digest] = struct{}{}
		}
	}

	return digests, unpinnedRepos, true
}

func parseImageRef(ref string) (repo, digest string, ok bool) {
	named, err := distreference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", "", false
	}

	if canonical, isCanonical := named.(distreference.Canonical); isCanonical {
		digest = canonical.Digest().String()
	}

	return distreference.FamiliarName(named), digest, true
}
