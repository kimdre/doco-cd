package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	swarmTypes "github.com/moby/moby/api/types/swarm"
	dockerClient "github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"

	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	composetypes "github.com/docker/cli/cli/compose/types"

	"github.com/kimdre/doco-cd/internal/docker/options"

	"github.com/kimdre/doco-cd/internal/webhook"
)

const (
	swarmResourceNameMaxLen = 64
	swarmHashSuffixLen      = 8
	swarmBaseNameMaxLen     = swarmResourceNameMaxLen - (1 + swarmHashSuffixLen) // "_" + hash
)

var (
	ErrNotAJobService                = errors.New("service is not a job-mode service")
	ErrJobServiceRestartNotSupported = errors.New("restart not supported for job services")
	swarmContentHashSuffixPattern    = regexp.MustCompile(fmt.Sprintf(`_[a-f0-9]{%d}$`, swarmHashSuffixLen))

	// ErrGlobalSwarmServiceNotScalable indicates a global/global-job service was
	// targeted by an operation that scales replicas, which Swarm does not allow.
	ErrGlobalSwarmServiceNotScalable = errors.New("global-mode swarm service cannot be scaled")
)

// LoadSwarmStack loads a Docker Swarm stack using the provided project and deploy configuration.
func LoadSwarmStack(dockerCli command.Cli, project *types.Project,
	deployConfig *deploy.Config, externalWorkingDir string,
) (*composetypes.Config, *options.Deploy, error) {
	opts := options.Deploy{
		Composefiles:     project.ComposeFiles,
		Namespace:        deployConfig.Name,
		ResolveImage:     swarmInternal.ResolveImageAlways,
		SendRegistryAuth: true,
		Prune:            deployConfig.RemoveOrphans,
		Detach:           false,
		Environment:      project.Environment,
	}

	cfg, err := swarmInternal.LoadComposefile(dockerCli, opts, deployConfig.Internal.Environment, externalWorkingDir)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load compose file: %w", err)
	}

	if err = SetConfigHashPrefixes(cfg, opts.Namespace); err != nil {
		return nil, nil, fmt.Errorf("failed to set config hash prefixes: %w", err)
	}

	if err = SetSecretHashPrefixes(cfg, opts.Namespace); err != nil {
		return nil, nil, fmt.Errorf("failed to set secret hash prefixes: %w", err)
	}

	return cfg, &opts, nil
}

// DeploySwarmStack deploys a Docker Swarm stack using the provided project and deploy configuration.
func DeploySwarmStack(ctx context.Context, dockerCli command.Cli, cfg *composetypes.Config, opts *options.Deploy) error {
	return swarmInternal.RunDeploy(ctx, dockerCli, opts, cfg)
}

// RemoveSwarmStack removes a Docker Swarm stack using the provided deploy configuration.
func RemoveSwarmStack(ctx context.Context, dockerCli command.Cli, namespace string) error {
	opts := options.Remove{
		Namespaces: []string{namespace},
		Detach:     false,
	}

	return swarmInternal.RunRemove(ctx, dockerCli, opts)
}

// stableSwarmMetadataLabels returns the subset of the deployment metadata that stays
// the same between deployments of an unchanged stack.
//
// These labels are safe to write into the task template (container labels, volume
// labels). Labels may only be added here if they change together with a change that
// legitimately recreates the tasks anyway (e.g. a renamed deployment or a re-pointed
// reference). Labels that differ between deployments of the same stack, such as the
// timestamp, the commit SHA or the source URL (which differs between webhook and poll
// triggers for the same repository), must never be added: swarm would recreate all
// tasks of every service on each deployment, see
// https://github.com/kimdre/doco-cd/issues/1153.
func stableSwarmMetadataLabels(deployConfig *deploy.Config, payload *webhook.ParsedPayload, repoDir string) map[string]string {
	return map[string]string{
		DocoCDLabels.Metadata.Manager:        app.Name,
		DocoCDLabels.Deployment.Name:         deployConfig.Name,
		DocoCDLabels.Deployment.WorkingDir:   repoDir,
		DocoCDLabels.Deployment.ConfigTarget: deployConfig.Internal.ConfigTarget,
		DocoCDLabels.Deployment.TargetRef:    ExtractOciArtifactTag(deployConfig.Reference),
		DocoCDLabels.Source.Type:             SourceTypeLabelValue(string(payload.Source), string(deployConfig.Source)),
		DocoCDLabels.Source.Name:             payload.FullName,
	}
}

// addSwarmServiceLabels adds custom labels to the services in a Docker Swarm stack.
//
// The full deployment metadata is set as service-level labels (ServiceSpec.Labels)
// via Deploy.Labels. Container labels are part of the task template, so updating
// them on every deployment would make swarm recreate the tasks of every service in
// the stack, even when nothing about the service actually changed.
//
// The stable subset of the metadata is additionally set as container labels (via
// s.Labels), so the containers remain identifiable via e.g.
// `docker ps --filter label=cd.doco.deployment.name=...` on worker nodes, where the
// service spec is not available.
func addSwarmServiceLabels(stack *composetypes.Config, deployConfig *deploy.Config, payload *webhook.ParsedPayload,
	repoDir, appVersion, timestamp, latestCommit, projectHash string,
) {
	stableLabels := stableSwarmMetadataLabels(deployConfig, payload, repoDir)

	serviceSpecLabels := map[string]string{
		DocoCDLabels.Metadata.Version:               appVersion,
		DocoCDLabels.Deployment.Timestamp:           timestamp,
		DocoCDLabels.Deployment.ComposeHash:         projectHash,
		DocoCDLabels.Deployment.Trigger:             payload.TriggerString(),
		DocoCDLabels.Deployment.CommitSHA:           latestCommit,
		DocoCDLabels.Deployment.ConfigHash:          deployConfig.Internal.Hash,
		DocoCDLabels.Deployment.AutoDiscovery:       strconv.FormatBool(deployConfig.AutoDiscovery.Enabled),
		DocoCDLabels.Deployment.AutoDiscoveryConfig: MarshalAutoDiscoveryConfig(deployConfig.AutoDiscovery),
		DocoCDLabels.Source.URL:                     payload.WebURL,
	}

	maps.Copy(serviceSpecLabels, stableLabels)

	for i, s := range stack.Services {
		if s.Deploy.Labels == nil {
			s.Deploy.Labels = make(map[string]string)
		}

		maps.Copy(s.Deploy.Labels, serviceSpecLabels)

		if s.Labels == nil {
			s.Labels = make(map[string]string)
		}

		maps.Copy(s.Labels, stableLabels)

		stack.Services[i] = s
	}
}

// addSwarmVolumeLabels adds custom labels to the volumes in a Docker Swarm stack.
//
// Volume labels end up in the mount options of the task template, so only the stable
// subset of the deployment metadata may be set here. Volumes are looked up by their
// stack namespace label and doco-cd labels are ignored when comparing volume configs,
// so deployment metadata such as the timestamp is intentionally left out.
func addSwarmVolumeLabels(stack *composetypes.Config, deployConfig *deploy.Config, payload *webhook.ParsedPayload,
	repoDir string,
) {
	customLabels := stableSwarmMetadataLabels(deployConfig, payload, repoDir)

	for i, v := range stack.Volumes {
		if v.Labels == nil {
			v.Labels = make(map[string]string)
		}

		maps.Copy(v.Labels, customLabels)

		stack.Volumes[i] = v
	}
}

// addSwarmConfigLabels adds custom labels to the configs in a Docker Swarm stack.
func addSwarmConfigLabels(stack *composetypes.Config, deployConfig *deploy.Config, payload *webhook.ParsedPayload,
	repoDir, appVersion, timestamp, latestCommit string,
) {
	customLabels := map[string]string{
		DocoCDLabels.Metadata.Manager:      app.Name,
		DocoCDLabels.Metadata.Version:      appVersion,
		DocoCDLabels.Deployment.Name:       deployConfig.Name,
		DocoCDLabels.Deployment.Timestamp:  timestamp,
		DocoCDLabels.Deployment.WorkingDir: repoDir,
		DocoCDLabels.Deployment.Trigger:    payload.TriggerString(),
		DocoCDLabels.Deployment.CommitSHA:  latestCommit,
		DocoCDLabels.Deployment.TargetRef:  ExtractOciArtifactTag(deployConfig.Reference),
		DocoCDLabels.Source.Type:           SourceTypeLabelValue(string(payload.Source), string(deployConfig.Source)),
		DocoCDLabels.Source.Name:           payload.FullName,
		DocoCDLabels.Source.URL:            payload.WebURL,
	}

	for i, c := range stack.Configs {
		if c.Labels == nil {
			c.Labels = make(map[string]string)
		}

		maps.Copy(c.Labels, customLabels)

		stack.Configs[i] = c
	}
}

func addSwarmSecretLabels(stack *composetypes.Config, deployConfig *deploy.Config, payload *webhook.ParsedPayload,
	repoDir, appVersion, timestamp, latestCommit string,
) {
	customLabels := map[string]string{
		DocoCDLabels.Metadata.Manager:      app.Name,
		DocoCDLabels.Metadata.Version:      appVersion,
		DocoCDLabels.Deployment.Name:       deployConfig.Name,
		DocoCDLabels.Deployment.Timestamp:  timestamp,
		DocoCDLabels.Deployment.WorkingDir: repoDir,
		DocoCDLabels.Deployment.Trigger:    payload.TriggerString(),
		DocoCDLabels.Deployment.CommitSHA:  latestCommit,
		DocoCDLabels.Deployment.TargetRef:  ExtractOciArtifactTag(deployConfig.Reference),
		DocoCDLabels.Source.Type:           SourceTypeLabelValue(string(payload.Source), string(deployConfig.Source)),
		DocoCDLabels.Source.Name:           payload.FullName,
		DocoCDLabels.Source.URL:            payload.WebURL,
	}

	for i, s := range stack.Secrets {
		if s.Labels == nil {
			s.Labels = make(map[string]string)
		}

		maps.Copy(s.Labels, customLabels)

		stack.Secrets[i] = s
	}
}

// SetConfigHashPrefixes generates hashes for the config definitions in the stack config
// and adds them to the config names as suffixes to trigger a redeployment when they change (Only works in Docker Swarm mode).
func SetConfigHashPrefixes(stack *composetypes.Config, namespace string) error {
	for i, c := range stack.Configs {
		if c.External.External {
			// Skip external configs, they are not managed by the stack
			continue
		}

		var content io.Reader

		contentBytes, err := os.ReadFile(c.File)
		if err != nil {
			return fmt.Errorf("failed to read config file %s: %w", c.File, err)
		}

		content = strings.NewReader(string(contentBytes))

		hash, err := generateShortHash(content)
		if err != nil {
			return fmt.Errorf("failed to generate hash for config %s: %w", c.Name, err)
		}

		if c.Name == "" {
			c.Name = fmt.Sprintf("%s_%s", namespace, filepath.Base(c.File))
		}

		if err := validateSwarmBaseNameLength(c.Name, "config"); err != nil {
			return err
		}

		oldName := c.Name
		nameWithHash := fmt.Sprintf("%s_%s", c.Name, hash)
		c.Name = nameWithHash
		stack.Configs[i] = c

		// Check for services that use this config and update their config references
		for j, service := range stack.Services {
			for k, cfg := range service.Configs {
				if cfg.Source == oldName {
					// Update the config reference in the service
					stack.Services[j].Configs[k].Source = nameWithHash
				}
			}
		}
	}

	return nil
}

// SetSecretHashPrefixes generates hashes for the secret definitions in the stack config
// and adds them to the secret names as suffixes to trigger a redeployment when they change (Only works in Docker Swarm mode).
func SetSecretHashPrefixes(stack *composetypes.Config, namespace string) error {
	for i, s := range stack.Secrets {
		if s.External.External {
			// Skip external secrets, they are not managed by the stack
			continue
		}

		var content io.Reader

		contentBytes, err := os.ReadFile(s.File)
		if err != nil {
			return fmt.Errorf("failed to read secret file %s: %w", s.File, err)
		}

		content = strings.NewReader(string(contentBytes))

		hash, err := generateShortHash(content)
		if err != nil {
			return fmt.Errorf("failed to generate hash for secret %s: %w", s.Name, err)
		}

		if s.Name == "" {
			s.Name = fmt.Sprintf("%s_%s", namespace, filepath.Base(s.File))
		}

		if err := validateSwarmBaseNameLength(s.Name, "secret"); err != nil {
			return err
		}

		oldName := s.Name
		nameWithHash := fmt.Sprintf("%s_%s", s.Name, hash)
		s.Name = nameWithHash
		stack.Secrets[i] = s

		// Check for services that use this secret and update their secret references
		for j, service := range stack.Services {
			for k, secret := range service.Secrets {
				if secret.Source == oldName {
					// Update the secret reference in the service
					stack.Services[j].Secrets[k].Source = nameWithHash
				}
			}
		}
	}

	return nil
}

// generateShortHash generates a short hash from the provided data reader.
func generateShortHash(data io.Reader) (hash string, err error) {
	const length = 8

	h := sha256.New()

	_, err = io.Copy(h, data)
	if err != nil {
		return "", fmt.Errorf("failed to generate hash: %w", err)
	}

	hash = hex.EncodeToString(h.Sum(nil))
	if len(hash) > length {
		hash = hash[:length] // Shorten hash to n characters
	} else if hash == "" {
		return "", errors.New("empty hash")
	}

	return hash, nil
}

// validateSwarmBaseNameLength checks if the base name of a Swarm resource (config or secret)
// is within the allowed length limit (swarmBaseNameMaxLen).
func validateSwarmBaseNameLength(name, resourceType string) error {
	if name == "" {
		return fmt.Errorf("%s name must not be empty", resourceType)
	}

	if len(name) <= swarmBaseNameMaxLen {
		return nil
	}

	return fmt.Errorf(
		"%s name %q is too long (%d chars): max %d chars allowed before hash suffix (%d chars) to stay within Docker Swarm limit (%d)",
		resourceType, name, len(name), swarmBaseNameMaxLen, swarmHashSuffixLen, swarmResourceNameMaxLen)
}

// stripSwarmContentHashSuffix removes the content hash suffix from a Swarm config or secret name.
func stripSwarmContentHashSuffix(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}

	return swarmContentHashSuffixPattern.ReplaceAllString(name, "")
}

// PruneStackConfigs removes old revisions of configs in a Docker Swarm stack,
// keeping only the specified number of recent revisions.
func PruneStackConfigs(ctx context.Context, client dockerClient.APIClient, namespace string, keepOldRevisions int) error {
	if keepOldRevisions < 0 {
		keepOldRevisions = 0
	}

	keepTotalRevisions := keepOldRevisions + 1

	// List all configs in the swarm
	configs, err := GetLabeledConfigs(ctx, client, swarmInternal.StackNamespaceLabel, namespace)
	if err != nil {
		return fmt.Errorf("failed to list configs: %w", err)
	}

	groupedConfigs := make(map[string][]swarmTypes.Config)

	for _, c := range configs {
		if c.Spec.Labels[swarmInternal.StackNamespaceLabel] == namespace {
			key := stripSwarmContentHashSuffix(c.Spec.Name)
			groupedConfigs[key] = append(groupedConfigs[key], c)
		}
	}

	for _, group := range groupedConfigs {
		sort.Slice(group, func(i, j int) bool {
			return group[i].Version.Index < group[j].Version.Index
		})

		removeTarget := len(group) - keepTotalRevisions
		if removeTarget <= 0 {
			continue
		}

		removed := 0
		for _, c := range group {
			if removed >= removeTarget {
				break
			}

			_, err = client.ConfigRemove(ctx, c.ID, dockerClient.ConfigRemoveOptions{})
			if err != nil {
				if strings.Contains(err.Error(), ErrIsInUse.Error()) {
					continue
				}

				return fmt.Errorf("failed to remove config %s: %w", c.ID, err)
			}

			removed++
		}
	}

	return nil
}

// PruneStackSecrets removes old revisions of secrets in a Docker Swarm stack,
// keeping only the specified number of recent revisions.
func PruneStackSecrets(ctx context.Context, client dockerClient.APIClient, namespace string, keepOldRevisions int) error {
	if keepOldRevisions < 0 {
		keepOldRevisions = 0
	}

	keepTotalRevisions := keepOldRevisions + 1

	// List all secrets in the swarm
	secrets, err := GetLabeledSecrets(ctx, client, swarmInternal.StackNamespaceLabel, namespace)
	if err != nil {
		return fmt.Errorf("failed to list secrets: %w", err)
	}

	groupedSecrets := make(map[string][]swarmTypes.Secret)

	for _, s := range secrets {
		if s.Spec.Labels[swarmInternal.StackNamespaceLabel] == namespace {
			key := stripSwarmContentHashSuffix(s.Spec.Name)
			groupedSecrets[key] = append(groupedSecrets[key], s)
		}
	}

	for _, group := range groupedSecrets {
		sort.Slice(group, func(i, j int) bool {
			return group[i].Version.Index < group[j].Version.Index
		})

		removeTarget := len(group) - keepTotalRevisions
		if removeTarget <= 0 {
			continue
		}

		removed := 0
		for _, s := range group {
			if removed >= removeTarget {
				break
			}

			_, err = client.SecretRemove(ctx, s.ID, dockerClient.SecretRemoveOptions{})
			if err != nil {
				if strings.Contains(err.Error(), ErrIsInUse.Error()) {
					continue
				}

				return fmt.Errorf("failed to remove secret %s: %w", s.ID, err)
			}

			removed++
		}
	}

	return nil
}

// WaitForSwarmService waits until a swarm service exists (and optionally has published ports).
func WaitForSwarmService(ctx context.Context, t *testing.T, cli dockerClient.APIClient, serviceName string, timeout time.Duration) (swarmTypes.Service, error) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	var lastErr error

	for time.Now().Before(deadline) {
		result, err := cli.ServiceInspect(ctx, serviceName, dockerClient.ServiceInspectOptions{
			InsertDefaults: true,
		})
		if err == nil {
			return result.Service, nil
		}

		lastErr = err

		time.Sleep(500 * time.Millisecond)
	}

	return swarmTypes.Service{}, fmt.Errorf("timed out waiting for service %s after %s: %w", serviceName, timeout.String(), lastErr)
}

// RestartService restarts long-running Swarm services by bumping ForceUpdate.
// For job-mode services (replicated-job/global-job), it returns ErrJobServiceRestartNotSupported.
func RestartService(ctx context.Context, cli dockerClient.APIClient, serviceName string) error {
	result, err := cli.ServiceInspect(ctx, serviceName, dockerClient.ServiceInspectOptions{
		InsertDefaults: true,
	})
	if err != nil {
		return fmt.Errorf("inspect service %s: %w", serviceName, err)
	}

	svc := result.Service

	// Job services cannot be updated with UpdateConfig present; treat restart as a no-op.
	if svc.Spec.Mode.ReplicatedJob != nil || svc.Spec.Mode.GlobalJob != nil {
		return ErrJobServiceRestartNotSupported
	}

	spec := svc.Spec
	if spec.TaskTemplate.ForceUpdate == 0 {
		spec.TaskTemplate.ForceUpdate = 1
	} else {
		spec.TaskTemplate.ForceUpdate++
	}

	_, err = cli.ServiceUpdate(ctx, svc.ID, dockerClient.ServiceUpdateOptions{
		Version: svc.Version,
		Spec:    spec,
	})
	if err != nil {
		return fmt.Errorf("update service %s: %w", serviceName, err)
	}

	return nil
}

// scheduledRestartReplicas returns the intended replica count for a restart-mode
// scheduled Swarm service, read from the JobRestartReplicas label that is set at
// deploy time when the service is pinned to 0 replicas. Defaults to 1.
func scheduledRestartReplicas(spec swarmTypes.ServiceSpec) uint64 {
	replicas := uint64(1)

	if spec.TaskTemplate.ContainerSpec == nil || spec.TaskTemplate.ContainerSpec.Labels == nil {
		return replicas
	}

	raw, ok := spec.TaskTemplate.ContainerSpec.Labels[docoCDJobLabelNames.JobRestartReplicas]
	if !ok {
		return replicas
	}

	if v, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64); err == nil && v > 0 {
		replicas = v
	}

	return replicas
}

// RestartScheduledSwarmService runs a restart-mode scheduled Swarm service on its
// schedule. Classic replicated scheduled services are deployed at 0 replicas so
// they do not run on deployment; this scales them up to their intended replica
// count (forcing a task re-run) when the schedule fires. Services that cannot be
// scaled to 0 (e.g. global) fall back to a ForceUpdate bump.
func RestartScheduledSwarmService(ctx context.Context, dockerCLI command.Cli, serviceName string) error {
	apiClient := dockerCLI.Client()

	result, err := apiClient.ServiceInspect(ctx, serviceName, dockerClient.ServiceInspectOptions{
		InsertDefaults: true,
	})
	if err != nil {
		return fmt.Errorf("inspect service %s: %w", serviceName, err)
	}

	svc := result.Service

	if svc.Spec.Mode.Replicated != nil {
		replicas := scheduledRestartReplicas(svc.Spec)
		return swarmInternal.ScaleService(ctx, dockerCLI, serviceName, replicas, false, true)
	}

	return RestartService(ctx, apiClient, serviceName)
}

// RerunJobService attempts to retrigger a Swarm job service (`replicated-job` or `global-job`)
// by updating the service spec (bumping a dummy label), causing Swarm to create new job tasks.
//
// Note: Swarm does not allow UpdateConfig / RollbackConfig on job-mode services, so we must
// strip those fields before calling ServiceUpdate.
func RerunJobService(ctx context.Context, cli dockerClient.APIClient, serviceName string) error {
	result, err := cli.ServiceInspect(ctx, serviceName, dockerClient.ServiceInspectOptions{
		InsertDefaults: true,
	})
	if err != nil {
		return fmt.Errorf("inspect service %s: %w", serviceName, err)
	}

	svc := result.Service

	isReplicatedJob := svc.Spec.Mode.ReplicatedJob != nil

	isGlobalJob := svc.Spec.Mode.GlobalJob != nil
	if !isReplicatedJob && !isGlobalJob {
		return ErrNotAJobService
	}

	spec := svc.Spec
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}

	// Change a no-op label to force a spec update.
	spec.Labels[docoCDJobLabelNames.JobLastRun] = time.Now().UTC().Format(time.RFC3339)

	// Jobs may not have an update config (daemon returns InvalidArgument otherwise).
	spec.UpdateConfig = nil
	spec.RollbackConfig = nil

	_, err = cli.ServiceUpdate(ctx, svc.ID, dockerClient.ServiceUpdateOptions{
		Version: svc.Version,
		Spec:    spec,
	})
	if err != nil {
		return fmt.Errorf("update (rerun) job service %s: %w", serviceName, err)
	}

	return nil
}

// ErrSwarmServiceAlreadyStopped indicates a replicated service was already
// scaled to 0 replicas, so there is nothing to stop or restore.
var ErrSwarmServiceAlreadyStopped = errors.New("swarm service is already scaled to 0 replicas")

// StopSwarmService temporarily stops a swarm service by scaling it to 0 replicas
// and waiting until its tasks have actually terminated.
// It returns the original replica count so the caller can restore it later via
// StartSwarmService.
//
// Waiting matters for the primary use case of this feature (consistent cold
// backups): ServiceUpdate only records the intent to scale down, so without
// waiting the scheduled job would start while the target's containers are
// still shutting down and flushing to disk.
//
// Global-mode services cannot be scaled to 0; the function returns
// (0, ErrGlobalSwarmServiceNotScalable) so the caller can skip them gracefully.
// A replicated service that is already at 0 replicas returns
// (0, ErrSwarmServiceAlreadyStopped).
//
// The serviceName must be the full swarm-scoped name (e.g. "mystack_myservice").
// In the cd.doco.job.stop_services label, cross-stack services are expressed as
// "stack/service" and resolved to "stack_service" before calling this function.
func StopSwarmService(ctx context.Context, dockerCLI command.Cli, serviceName string, timeout time.Duration) (originalReplicas uint64, err error) {
	result, err := dockerCLI.Client().ServiceInspect(ctx, serviceName, dockerClient.ServiceInspectOptions{
		InsertDefaults: true,
	})
	if err != nil {
		return 0, fmt.Errorf("inspect service %s: %w", serviceName, err)
	}

	svc := result.Service

	// Global and global-job services cannot be scaled to 0.
	if svc.Spec.Mode.Global != nil || svc.Spec.Mode.GlobalJob != nil {
		return 0, ErrGlobalSwarmServiceNotScalable
	}

	// Determine the current (intended) replica count.
	var replicas uint64

	switch {
	case svc.Spec.Mode.Replicated != nil && svc.Spec.Mode.Replicated.Replicas != nil:
		replicas = *svc.Spec.Mode.Replicated.Replicas
	case svc.Spec.Mode.ReplicatedJob != nil && svc.Spec.Mode.ReplicatedJob.TotalCompletions != nil:
		replicas = *svc.Spec.Mode.ReplicatedJob.TotalCompletions
	default:
		replicas = 1
	}

	if replicas == 0 {
		return 0, ErrSwarmServiceAlreadyStopped
	}

	// Scale to 0.
	if err := swarmInternal.ScaleService(ctx, dockerCLI, serviceName, 0, false, false); err != nil {
		return 0, fmt.Errorf("scale service %s to 0: %w", serviceName, err)
	}

	if err := waitForSwarmServiceTasksStopped(ctx, dockerCLI, svc.ID, serviceName, timeout); err != nil {
		return replicas, err
	}

	return replicas, nil
}

// waitForSwarmServiceTasksStopped blocks until the given service has no tasks
// left in a live state, or until timeout elapses.
//
// swarm.ScaleService(wait=true) is not sufficient here: it delegates to the
// Docker CLI progress writer, which short-circuits for a service scaled to 0
// and therefore returns before the tasks have actually shut down.
func waitForSwarmServiceTasksStopped(ctx context.Context, dockerCLI command.Cli, serviceID, serviceName string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		tasks, err := dockerCLI.Client().TaskList(waitCtx, dockerClient.TaskListOptions{
			Filters: make(dockerClient.Filters).Add("service", serviceID),
		})
		if err != nil {
			if waitCtx.Err() != nil && ctx.Err() == nil {
				return fmt.Errorf("timed out after %s waiting for task(s) of service %s to stop", timeout, serviceName)
			}

			return fmt.Errorf("list tasks of service %s: %w", serviceName, err)
		}

		live := 0

		for _, task := range tasks.Items {
			// A task still occupies resources (and may still be writing to
			// volumes) until it reaches a terminal state.
			switch task.Status.State {
			case swarmTypes.TaskStateShutdown, swarmTypes.TaskStateComplete,
				swarmTypes.TaskStateFailed, swarmTypes.TaskStateRejected,
				swarmTypes.TaskStateOrphaned, swarmTypes.TaskStateRemove:
				continue
			default:
				live++
			}
		}

		if live == 0 {
			return nil
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out after %s waiting for %d task(s) of service %s to stop", timeout, live, serviceName)
		case <-ticker.C:
		}
	}
}

// StartSwarmService restores a previously stopped swarm service to the given
// replica count. It is the counterpart to StopSwarmService and is typically
// called in a deferred function to guarantee the service is restarted even when
// the scheduled job fails.
//
// A replicas value of 0 means StopSwarmService never actually stopped the
// service (global-mode, or already scaled to 0), so there is nothing to restore.
//
// The serviceName must be the full swarm-scoped name (e.g. "mystack_myservice").
func StartSwarmService(ctx context.Context, dockerCLI command.Cli, serviceName string, replicas uint64) error {
	if replicas == 0 {
		return nil
	}

	if err := swarmInternal.ScaleService(ctx, dockerCLI, serviceName, replicas, false, false); err != nil {
		return fmt.Errorf("scale service %s back to %d: %w", serviceName, replicas, err)
	}

	return nil
}
