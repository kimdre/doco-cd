package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"

	"github.com/moby/moby/api/types/volume"

	"go.yaml.in/yaml/v4"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"

	"github.com/moby/moby/client"
)

var (
	ErrMountPointNotFound     = errors.New("mount point not found")
	ErrMountPointNotWriteable = errors.New("mount point is not writeable")
	ErrContainerIDNotFound    = errors.New("container ID not found")
)

// GetContainerID retrieves the container ID for a given service name.
func GetContainerID(apiClient client.APIClient, name string) (id string, err error) {
	result, err := apiClient.ContainerList(context.TODO(), client.ContainerListOptions{All: true})
	if err != nil {
		return "", err
	}

	for _, cont := range result.Items {
		for _, containerName := range cont.Names {
			if strings.Contains(containerName, name) { // Match by service name
				return cont.ID, nil
			}
		}
	}

	return "", fmt.Errorf("%w: %s", ErrContainerIDNotFound, name)
}

type (
	Service string            // Name of the Service
	Labels  map[string]string // Labels of the Service
)

func (l Labels) Get(key string) (string, bool) {
	v, ok := l[key]
	return v, ok
}

func (l Labels) getDeploymentCommitSHA() (string, bool) {
	return l.Get(DocoCDLabels.Deployment.CommitSHA)
}

func (l Labels) getDeploymentComposeHash() (string, bool) {
	return l.Get(DocoCDLabels.Deployment.ComposeHash)
}

// SwarmServiceLabels returns the labels of a swarm service, merged from the task
// template and the service spec.
//
// Deployment metadata is stored in the service spec labels, because labels in the
// task template are part of the service definition and changing them makes swarm
// recreate all tasks of the service on every deployment.
//
// Stacks deployed by earlier versions carry the metadata in the container spec
// instead, so both label sets are merged, with the service spec taking precedence.
// Note that this only helps sites that read labels from an inspected service:
// server-side filtered listings (e.g. GetServicesByLabel) match service spec labels
// only, so legacy stacks stay invisible to them until they are redeployed once.
// The container spec fallback can be dropped in a future release, once stacks have
// been redeployed at least once.
func SwarmServiceLabels(service swarm.Service) Labels {
	containerLabels := swarmContainerLabels(service)

	labels := make(Labels, len(containerLabels)+len(service.Spec.Labels))
	maps.Copy(labels, containerLabels)
	maps.Copy(labels, service.Spec.Labels)

	return labels
}

// SwarmJobLabels returns the labels of a swarm service with the job configuration
// taken from the task template only.
//
// Job configuration labels are authored by the user as service labels in the compose
// file and are therefore part of the task template, which is also where the deploy
// path reads them from. Honoring them from the service spec here would schedule
// services that are not set up as jobs during deployment.
//
// Job runtime metadata (last/next run timestamps) is written by doco-cd itself to the
// service spec, so it is exempt from this rule.
func SwarmJobLabels(service swarm.Service) Labels {
	containerLabels := swarmContainerLabels(service)

	labels := make(Labels, len(containerLabels)+len(service.Spec.Labels))
	maps.Copy(labels, containerLabels)

	for key, value := range service.Spec.Labels {
		if isJobConfigLabel(key) {
			continue
		}

		labels[key] = value
	}

	return labels
}

// isJobConfigLabel reports whether a label key configures a scheduled job, as opposed
// to job runtime metadata maintained by doco-cd.
func isJobConfigLabel(key string) bool {
	if !strings.HasPrefix(key, jobLabelPrefix) {
		return false
	}

	return key != docoCDJobLabelNames.JobLastRun && key != docoCDJobLabelNames.JobNextRun
}

func swarmContainerLabels(service swarm.Service) map[string]string {
	if service.Spec.TaskTemplate.ContainerSpec == nil {
		return nil
	}

	return service.Spec.TaskTemplate.ContainerSpec.Labels
}

// GetServiceLabels retrieves the Labels for each Service in a given stack.
func GetServiceLabels(ctx context.Context, cli client.APIClient, swarmMode bool, stackName string) (map[Service]Labels, error) {
	if swarmMode {
		services, err := swarmInternal.GetStackServices(ctx, cli, stackName)
		if err != nil {
			return nil, fmt.Errorf("failed to get services for stack %s: %w", stackName, err)
		}

		result := make(map[Service]Labels)
		for _, service := range services {
			result[Service(service.Spec.Name)] = SwarmServiceLabels(service)
		}

		return result, nil
	}

	containers, err := GetLabeledContainers(ctx, cli, api.ProjectLabel, stackName, false)
	if err != nil {
		return nil, fmt.Errorf("failed to get containers for stack %s: %w", stackName, err)
	}

	result := make(map[Service]Labels)
	for _, cont := range containers {
		result[Service(cont.Names[0])] = cont.Labels
	}

	return result, nil
}

// GetLabeledContainers retrieves all containers with a specific label key and value.
func GetLabeledContainers(ctx context.Context, cli client.APIClient, key, value string, all bool) (containers []container.Summary, err error) {
	result, err := cli.ContainerList(ctx, client.ContainerListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
		All:     all,
	})
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}

// GetLabeledServices retrieves all services with a specific label key and value, along with their labels.
func GetLabeledServices(ctx context.Context, cli client.APIClient, swarmMode bool, key, value string) (map[Service]map[string]string, error) {
	if swarmMode {
		services, err := swarmInternal.GetServicesByLabel(ctx, cli, key, value)
		if err != nil {
			return nil, fmt.Errorf("failed to get services with label %s=%s: %w", key, value, err)
		}

		result := make(map[Service]map[string]string)
		for _, service := range services {
			result[Service(service.Spec.Name)] = SwarmServiceLabels(service)
		}

		return result, nil
	}

	containers, err := GetLabeledContainers(ctx, cli, key, value, false)
	if err != nil {
		return nil, fmt.Errorf("failed to get containers with label %s=%s: %w", key, value, err)
	}

	result := make(map[Service]map[string]string)
	for _, cont := range containers {
		result[Service(cont.Names[0])] = cont.Labels
	}

	return result, nil
}

// GetLabeledVolumes retrieves all volumes with a specific label key and value.
func GetLabeledVolumes(ctx context.Context, cli client.APIClient, key, value string) (volumes []volume.Volume, err error) {
	volResp, err := cli.VolumeList(ctx, client.VolumeListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list volumes with label %s=%s: %w", key, value, err)
	}

	return volResp.Items, nil
}

// GetLabeledConfigs retrieves all configs with a specific label key and value.
func GetLabeledConfigs(ctx context.Context, cli client.APIClient, key, value string) (configs []swarm.Config, err error) {
	result, err := cli.ConfigList(ctx, client.ConfigListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list configs with label %s=%s: %w", key, value, err)
	}

	return result.Items, nil
}

// GetLabeledSecrets retrieves all secrets with a specific label key and value.
func GetLabeledSecrets(ctx context.Context, cli client.APIClient, key, value string) (secrets []swarm.Secret, err error) {
	result, err := cli.SecretList(ctx, client.SecretListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list secrets with label %s=%s: %w", key, value, err)
	}

	return result.Items, nil
}

// GetMountPointByDestination retrieves the mount point of a container volume/bind mount by its destination (mount point inside the container).
func GetMountPointByDestination(cli client.APIClient, containerID, destination string) (container.MountPoint, error) {
	// Get the container info
	result, err := cli.ContainerInspect(context.TODO(), containerID, client.ContainerInspectOptions{})
	if err != nil {
		return container.MountPoint{}, fmt.Errorf("failed to inspect container %s: %w", containerID, err)
	}

	// Get the volume path
	for _, mount := range result.Container.Mounts {
		if mount.Destination == destination {
			return mount, nil
		}
	}

	return container.MountPoint{}, fmt.Errorf("%w: %s", ErrMountPointNotFound, destination)
}

// CheckMountPointWriteable checks if a mount point is writable by attempting to create a file in it.
func CheckMountPointWriteable(mountPoint container.MountPoint) error {
	if !mountPoint.RW {
		return fmt.Errorf("%w: %s", ErrMountPointNotWriteable, mountPoint.Destination)
	}

	// Create a test file to check if the mount point is writable
	testFilePath := filepath.Join(mountPoint.Destination, ".test")

	f, err := os.Create(testFilePath)
	if err != nil {
		return fmt.Errorf("failed to create file in %s: %w", testFilePath, err)
	}

	defer f.Close()

	defer func() {
		err = os.Remove(testFilePath)
		if err != nil {
			fmt.Printf("failed to remove test file %s: %v\n", testFilePath, err)
		}
	}()

	return nil
}

func RemoveLabeledVolumes(ctx context.Context, dockerClient client.APIClient, swarmMode bool, stackName string) error {
	filterLabel := api.ProjectLabel
	if swarmMode {
		filterLabel = swarmInternal.StackNamespaceLabel
	}

	volumes, err := GetLabeledVolumes(ctx, dockerClient, filterLabel, stackName)
	if err != nil {
		return fmt.Errorf("failed to get labeled volumes: %w", err)
	}

	for _, vol := range volumes {
		retries := 5

		for range retries {
			_, err = dockerClient.VolumeRemove(ctx, vol.Name, client.VolumeRemoveOptions{Force: true})
			if err != nil {
				if strings.Contains(err.Error(), ErrIsInUse.Error()) {
					time.Sleep(2 * time.Second)
					continue
				}

				return fmt.Errorf("failed to remove volume %s: %w", vol.Name, err)
			}

			break
		}
	}

	return nil
}

// MarshalAutoDiscoveryConfig serializes an AutoDiscoveryConfig to a single-line YAML flow-style string for use as a container label.
func MarshalAutoDiscoveryConfig(cfg deployConfig.AutoDiscoveryConfig) string {
	// First marshal to get standard YAML
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return "{}"
	}

	// Parse it back into a node to apply flow style
	var node yaml.Node
	if err := yaml.Unmarshal(b, &node); err != nil {
		return "{}"
	}

	// Set flow style (produces {key: value, key: value} format)
	if len(node.Content) > 0 {
		node.Content[0].Style = yaml.FlowStyle
	}

	// Marshal with flow style applied
	result, err := yaml.Marshal(&node)
	if err != nil {
		return "{}"
	}

	return strings.TrimSpace(string(result))
}

// ParseAutoDiscoveryConfig deserializes an AutoDiscoveryConfig from a YAML container label value.
// If the label is empty or invalid it returns a default config with Delete=true, RemoveVolumes=false, RemoveImages=true.
func ParseAutoDiscoveryConfig(labelValue string) deployConfig.AutoDiscoveryConfig {
	defaults := deployConfig.AutoDiscoveryConfig{
		Delete:        true,
		RemoveVolumes: false,
		RemoveImages:  true,
	}

	if labelValue == "" {
		return defaults
	}

	var cfg deployConfig.AutoDiscoveryConfig
	if err := yaml.Unmarshal([]byte(labelValue), &cfg); err != nil {
		return defaults
	}

	return cfg
}
