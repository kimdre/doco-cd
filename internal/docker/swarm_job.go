package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/mount"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

// JobMode represents the mode of a Docker Swarm job.
type JobMode string

const (
	JobModeGlobal     JobMode = "global-job"
	JobModeReplicated JobMode = "replicated-job"
)

// RunSwarmJob runs a Docker Swarm job container with the specified mode and command.
// https://docs.docker.com/reference/cli/docker/service/create/#running-as-a-job
func RunSwarmJob(ctx context.Context, dockerCLI command.Cli, mode JobMode, command []string, title string) error {
	apiClient := dockerCLI.Client()

	var (
		serviceMode swarmTypes.ServiceMode
		serviceId   string
	)

	switch mode {
	case JobModeGlobal:
		serviceMode = swarmTypes.ServiceMode{
			GlobalJob: &swarmTypes.GlobalJob{},
		}
	case JobModeReplicated:
		serviceMode = swarmTypes.ServiceMode{
			ReplicatedJob: &swarmTypes.ReplicatedJob{},
		}
	default:
		return fmt.Errorf("unsupported job mode: %s", mode)
	}

	if title == "" {
		title = "helper-job"
	}

	newServiceSpec := swarmTypes.ServiceSpec{
		Annotations: swarmTypes.Annotations{
			Name: fmt.Sprintf("%s_%s", config.AppName, title),
			Labels: map[string]string{
				DocoCDLabels.Metadata.Manager:   config.AppName,
				DocoCDLabels.Metadata.Version:   config.AppVersion,
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

			inspectResult, getErr := apiClient.ServiceInspect(ctx, serviceId, client.ServiceInspectOptions{})
			if getErr != nil {
				return fmt.Errorf("error inspecting existing service: %w", getErr)
			}

			existingService := inspectResult.Service

			// Update the ForceUpdate to trigger a new job run
			existingService.Spec.TaskTemplate.ContainerSpec.Labels = newServiceSpec.TaskTemplate.ContainerSpec.Labels
			existingService.Spec.TaskTemplate.ContainerSpec.Command = newServiceSpec.TaskTemplate.ContainerSpec.Command
			existingService.Spec.TaskTemplate.ForceUpdate = newServiceSpec.TaskTemplate.ForceUpdate

			_, updateErr := apiClient.ServiceUpdate(ctx, serviceId, client.ServiceUpdateOptions{
				Version:       existingService.Version,
				Spec:          existingService.Spec,
				QueryRegistry: true,
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
	return RunSwarmJob(ctx, dockerCLI, JobModeGlobal, []string{"docker", "image", "prune", "--force"}, "image-prune")
}

// RunImageRemoveJob runs a Docker Swarm global job to remove specified images.
func RunImageRemoveJob(ctx context.Context, dockerCLI command.Cli, images []string) error {
	args := append([]string{"docker", "image", "rm", "--force"}, images...)
	return RunSwarmJob(ctx, dockerCLI, JobModeGlobal, args, "image-remove")
}
