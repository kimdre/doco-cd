package docker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/config/app"

	"github.com/moby/moby/api/types/mount"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

var swarmJobLock = sync.Map{}

func getSwarmJobLock(name string) *sync.Mutex {
	lock, _ := swarmJobLock.LoadOrStore(name, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// RunSwarmJob runs a Docker Swarm job container with the specified mode and command.
// https://docs.docker.com/reference/cli/docker/service/create/#running-as-a-job
func RunSwarmJob(ctx context.Context, dockerCLI command.Cli, mode swarm.DeployMode, command []string, title string) error {
	apiClient := dockerCLI.Client()

	var (
		serviceMode swarmTypes.ServiceMode
		serviceId   string
	)

	switch mode {
	case swarm.DeployModeGlobalJob:
		serviceMode = swarmTypes.ServiceMode{
			GlobalJob: &swarmTypes.GlobalJob{},
		}
	case swarm.DeployModeReplicatedJob:
		serviceMode = swarmTypes.ServiceMode{
			ReplicatedJob: &swarmTypes.ReplicatedJob{},
		}
	default:
		return fmt.Errorf("unsupported job mode: %s", mode)
	}

	if title == "" {
		title = "helper-job"
	}
	// fix conflict error
	// Error response from daemon: rpc error: code = Unknown desc = update out of sequence

	name := fmt.Sprintf("%s_%s", app.Name, title)

	lock := getSwarmJobLock(name)
	lock.Lock()
	defer lock.Unlock()

	newServiceSpec := swarmTypes.ServiceSpec{
		Annotations: swarmTypes.Annotations{
			Name: name,
			Labels: map[string]string{
				DocoCDLabels.Metadata.Manager:   app.Name,
				DocoCDLabels.Metadata.Version:   app.Version,
				DocoCDLabels.Deployment.Trigger: title,
			},
		},
		TaskTemplate: swarmTypes.TaskSpec{
			ContainerSpec: &swarmTypes.ContainerSpec{
				Image:   "docker:cli",
				Command: command,
				Mounts: []mount.Mount{
					{
						Type:   mount.TypeBind,
						Source: SocketPath,
						Target: SocketPath,
					},
				},
			},
			RestartPolicy: &swarmTypes.RestartPolicy{
				Condition: swarmTypes.RestartPolicyConditionNone,
			},
			ForceUpdate: uint64(time.Now().Unix()), // #nosec G115
		},
		Mode: serviceMode,
	}

	response, err := apiClient.ServiceCreate(ctx, client.ServiceCreateOptions{
		Spec:          newServiceSpec,
		QueryRegistry: true,
	})
	if err == nil {
		serviceId = response.ID
	} else {
		// Update existing service to trigger a new job run
		if strings.Contains(err.Error(), "already exists") {
			// Get the existing service ID
			filter := make(client.Filters).Add("name", newServiceSpec.Name)

			listResult, listErr := apiClient.ServiceList(ctx, client.ServiceListOptions{Filters: filter})
			if listErr != nil {
				return fmt.Errorf("error listing services: %w", listErr)
			}

			if len(listResult.Items) == 0 {
				return errors.New("service already exists but could not find it")
			}

			for _, service := range listResult.Items {
				if service.Spec.Name == newServiceSpec.Name {
					serviceId = service.ID
					break
				}
			}

			if serviceId == "" {
				return errors.New("service already exists but could not find its ID")
			}

			updateErr := retry.New(
				retry.Attempts(5),
				retry.Delay(250*time.Millisecond),
				retry.DelayType(retry.BackOffDelay),
				retry.RetryIf(func(err error) bool {
					return strings.Contains(err.Error(), "update out of sequence")
				}),
			).Do(
				func() error {
					inspectResult, getErr := apiClient.ServiceInspect(ctx, serviceId, client.ServiceInspectOptions{})
					if getErr != nil {
						return fmt.Errorf("error inspecting existing service: %w", getErr)
					}

					existingService := inspectResult.Service

					// already up to date, no need to update
					if existingService.Spec.TaskTemplate.ForceUpdate == newServiceSpec.TaskTemplate.ForceUpdate &&
						reflect.DeepEqual(existingService.Spec.TaskTemplate.ContainerSpec.Labels, newServiceSpec.TaskTemplate.ContainerSpec.Labels) &&
						reflect.DeepEqual(existingService.Spec.TaskTemplate.ContainerSpec.Command, newServiceSpec.TaskTemplate.ContainerSpec.Command) {
						return nil
					}
					// Update the ForceUpdate to trigger a new job run
					existingService.Spec.TaskTemplate.ContainerSpec.Labels = newServiceSpec.TaskTemplate.ContainerSpec.Labels
					existingService.Spec.TaskTemplate.ContainerSpec.Command = newServiceSpec.TaskTemplate.ContainerSpec.Command
					existingService.Spec.TaskTemplate.ForceUpdate = newServiceSpec.TaskTemplate.ForceUpdate

					_, updateErr := apiClient.ServiceUpdate(ctx, serviceId, client.ServiceUpdateOptions{
						Version:       existingService.Version,
						Spec:          existingService.Spec,
						QueryRegistry: true,
					})

					return updateErr
				})
			if updateErr != nil {
				return fmt.Errorf("error updating existing service: %w", updateErr)
			}
		} else {
			return fmt.Errorf("error creating one-off job service: %w", err)
		}
	}

	// defer func() {
	//	// Remove the service after completion
	//	_ = apiClient.ServiceRemove(ctx, response.ID)
	// }()

	// Wait for container to complete
	err = swarm.WaitOnServices(ctx, dockerCLI, []string{serviceId})
	if err != nil {
		return fmt.Errorf("error waiting for one-off job service: %w", err)
	}

	return nil
}

// RunImagePruneJob runs a Docker Swarm global job to prune unused images on all nodes.
func RunImagePruneJob(ctx context.Context, dockerCLI command.Cli) error {
	return RunSwarmJob(ctx, dockerCLI, swarm.DeployModeGlobalJob, []string{"docker", "image", "prune", "--force"}, "image-prune")
}

// RunImageRemoveJob runs a Docker Swarm global job to remove specified images.
func RunImageRemoveJob(ctx context.Context, dockerCLI command.Cli, images []string) error {
	args := append([]string{"docker", "image", "rm", "--force"}, images...)
	return RunSwarmJob(ctx, dockerCLI, swarm.DeployModeGlobalJob, args, "image-remove")
}

type SwarmOneShotFromServiceOptions struct {
	Replicas         uint64
	SendRegistryAuth bool
}

// RunSwarmOneShotFromService creates a temporary job service from an existing service spec and waits for completion.
func RunSwarmOneShotFromService(ctx context.Context, dockerCLI command.Cli, serviceName string, opts SwarmOneShotFromServiceOptions) error {
	apiClient := dockerCLI.Client()

	if opts.Replicas == 0 {
		opts.Replicas = 1
	}

	inspectResult, err := apiClient.ServiceInspect(ctx, serviceName, client.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect service %s: %w", serviceName, err)
	}

	sourceService := inspectResult.Service
	oneShotSpec := sourceService.Spec
	oneShotSpec.Name = fmt.Sprintf("%s-doco-job-%d", sourceService.Spec.Name, time.Now().UTC().UnixNano())

	if oneShotSpec.TaskTemplate.ContainerSpec == nil {
		return fmt.Errorf("service %s has no task container spec", serviceName)
	}

	if oneShotSpec.Labels == nil {
		oneShotSpec.Labels = map[string]string{}
	}

	oneShotSpec.Labels[DocoCDLabels.Metadata.Manager] = app.Name
	oneShotSpec.Labels[DocoCDLabels.Deployment.Trigger] = "job.schedule"

	if sourceService.Spec.Mode.Global != nil || sourceService.Spec.Mode.GlobalJob != nil {
		oneShotSpec.Mode = swarmTypes.ServiceMode{
			GlobalJob: &swarmTypes.GlobalJob{},
		}
	} else {
		oneShotSpec.Mode = swarmTypes.ServiceMode{
			ReplicatedJob: &swarmTypes.ReplicatedJob{
				TotalCompletions: &opts.Replicas,
				MaxConcurrent:    &opts.Replicas,
			},
		}
	}

	oneShotSpec.UpdateConfig = nil
	oneShotSpec.RollbackConfig = nil
	oneShotSpec.TaskTemplate.RestartPolicy = &swarmTypes.RestartPolicy{
		Condition: swarmTypes.RestartPolicyConditionNone,
	}

	createOpts := client.ServiceCreateOptions{
		Spec: oneShotSpec,
	}

	if opts.SendRegistryAuth {
		encodedAuth, authErr := command.RetrieveAuthTokenFromImage(dockerCLI.ConfigFile(), oneShotSpec.TaskTemplate.ContainerSpec.Image)
		if authErr != nil {
			return fmt.Errorf("retrieve auth token from image: %w", authErr)
		}

		createOpts.EncodedRegistryAuth = encodedAuth
	}

	createResult, err := apiClient.ServiceCreate(ctx, createOpts)
	if err != nil {
		return fmt.Errorf("create one-shot service from %s: %w", serviceName, err)
	}

	defer func() {
		_, _ = apiClient.ServiceRemove(context.WithoutCancel(ctx), createResult.ID, client.ServiceRemoveOptions{})
	}()

	if err = swarm.WaitOnServices(ctx, dockerCLI, []string{createResult.ID}); err != nil {
		return fmt.Errorf("wait one-shot service %s: %w", createResult.ID, err)
	}

	return nil
}
