package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/config/app"

	"github.com/moby/moby/api/types/mount"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker/registryauth"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

var swarmJobLock = sync.Map{}

const swarmOneOffCleanupTimeout = 30 * time.Second

func getSwarmJobLock(name string) *sync.Mutex {
	lock, _ := swarmJobLock.LoadOrStore(name, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// RunSwarmJob runs a Docker Swarm job container with the specified mode and command.
// https://docs.docker.com/reference/cli/docker/service/create/#running-as-a-job
func RunSwarmJob(ctx context.Context, dockerCLI command.Cli, mode swarm.DeployMode, command []string, title string) error {
	apiClient := dockerCLI.Client()

	var (
		serviceMode          swarmTypes.ServiceMode
		serviceID            string
		previousJobIteration *uint64
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
		Name: name,
		Labels: map[string]string{
			DocoCDLabels.Metadata.Manager:   app.Name,
			DocoCDLabels.Metadata.Version:   app.Version,
			DocoCDLabels.Deployment.Trigger: title,
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
		serviceID = response.ID
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
					serviceID = service.ID
					break
				}
			}

			if serviceID == "" {
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
					inspectResult, getErr := apiClient.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
					if getErr != nil {
						return fmt.Errorf("error inspecting existing service: %w", getErr)
					}

					existingService := inspectResult.Service
					if existingService.JobStatus != nil {
						iteration := existingService.JobStatus.JobIteration.Index
						previousJobIteration = &iteration
					}

					// Update ForceUpdate monotonically so every invocation starts a
					// new job iteration, including multiple runs in one second.
					existingService.Spec.TaskTemplate.ContainerSpec.Labels = newServiceSpec.TaskTemplate.ContainerSpec.Labels
					existingService.Spec.TaskTemplate.ContainerSpec.Command = newServiceSpec.TaskTemplate.ContainerSpec.Command
					existingService.Spec.TaskTemplate.ForceUpdate++

					_, updateErr := apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
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

	// Wait for the job's current iteration to complete or fail.
	err = swarm.WaitOnJobService(ctx, dockerCLI, serviceID, previousJobIteration)
	if err != nil {
		return fmt.Errorf("error waiting for job service: %w", err)
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

type SwarmOneOffFromServiceOptions struct {
	Replicas         uint64
	SendRegistryAuth bool
}

// RunSwarmOneOffFromService creates a temporary job service from an existing service spec and waits for completion.
func RunSwarmOneOffFromService(ctx context.Context, dockerCLI command.Cli, serviceName string, opts SwarmOneOffFromServiceOptions) (err error) {
	apiClient := dockerCLI.Client()

	if opts.Replicas == 0 {
		opts.Replicas = 1
	}

	inspectResult, err := apiClient.ServiceInspect(ctx, serviceName, client.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect service %s: %w", serviceName, err)
	}

	sourceService := inspectResult.Service
	oneOffSpec := sourceService.Spec
	oneOffSpec.Name = fmt.Sprintf("%s-doco-job-%d", sourceService.Spec.Name, time.Now().UTC().UnixNano())

	if oneOffSpec.TaskTemplate.ContainerSpec == nil {
		return fmt.Errorf("service %s has no task container spec", serviceName)
	}

	// Copy the nested reference fields before changing the clone. A direct
	// ServiceSpec assignment shares the source service's label maps and
	// ContainerSpec pointer.
	containerSpec := *oneOffSpec.TaskTemplate.ContainerSpec
	containerSpec.Labels = maps.Clone(containerSpec.Labels)
	oneOffSpec.TaskTemplate.ContainerSpec = &containerSpec
	oneOffSpec.Labels = maps.Clone(oneOffSpec.Labels)

	if oneOffSpec.TaskTemplate.ContainerSpec.Labels == nil {
		oneOffSpec.TaskTemplate.ContainerSpec.Labels = map[string]string{}
	}

	if oneOffSpec.Labels == nil {
		oneOffSpec.Labels = map[string]string{}
	}

	for _, labels := range []map[string]string{
		oneOffSpec.TaskTemplate.ContainerSpec.Labels,
		oneOffSpec.Labels,
	} {
		labels[DocoCDJobLabels.JobEphemeral] = "true"
		labels[DocoCDJobLabels.JobSourceServiceID] = sourceService.ID
	}

	oneOffSpec.Labels[DocoCDLabels.Metadata.Manager] = app.Name
	oneOffSpec.Labels[DocoCDLabels.Deployment.Trigger] = "job.schedule"

	if sourceService.Spec.Mode.Global != nil || sourceService.Spec.Mode.GlobalJob != nil {
		oneOffSpec.Mode = swarmTypes.ServiceMode{
			GlobalJob: &swarmTypes.GlobalJob{},
		}
	} else {
		oneOffSpec.Mode = swarmTypes.ServiceMode{
			ReplicatedJob: &swarmTypes.ReplicatedJob{
				TotalCompletions: &opts.Replicas,
				MaxConcurrent:    &opts.Replicas,
			},
		}
	}

	oneOffSpec.UpdateConfig = nil
	oneOffSpec.RollbackConfig = nil
	oneOffSpec.TaskTemplate.RestartPolicy = &swarmTypes.RestartPolicy{
		Condition: swarmTypes.RestartPolicyConditionNone,
	}

	createOpts := client.ServiceCreateOptions{
		Spec: oneOffSpec,
	}

	if opts.SendRegistryAuth {
		encodedAuth, authErr := command.RetrieveAuthTokenFromImage(dockerCLI.ConfigFile(), oneOffSpec.TaskTemplate.ContainerSpec.Image)
		if authErr != nil {
			return registryauth.WrapLookupError(dockerCLI.ConfigFile(), oneOffSpec.TaskTemplate.ContainerSpec.Image, authErr)
		}

		createOpts.EncodedRegistryAuth = encodedAuth
	}

	createResult, err := apiClient.ServiceCreate(ctx, createOpts)
	if err != nil {
		return fmt.Errorf("create one-off service from %s: %w", serviceName, err)
	}

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), swarmOneOffCleanupTimeout)
		defer cancel()

		_, cleanupErr := apiClient.ServiceRemove(cleanupCtx, createResult.ID, client.ServiceRemoveOptions{})
		if cleanupErr == nil || errdefs.IsNotFound(cleanupErr) {
			return
		}

		cleanupErr = fmt.Errorf("remove one-off service %s: %w", createResult.ID, cleanupErr)
		if err == nil {
			err = cleanupErr
		} else {
			err = errors.Join(err, cleanupErr)
		}
	}()

	if err = swarm.WaitOnJobService(ctx, dockerCLI, createResult.ID, nil); err != nil {
		return fmt.Errorf("wait one-off service %s: %w", createResult.ID, err)
	}

	return nil
}
