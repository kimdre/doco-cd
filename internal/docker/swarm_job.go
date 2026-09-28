package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/common/types/clone"
	"github.com/kimdre/doco-cd/internal/config/app"

	"github.com/moby/moby/api/types/mount"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker/registryauth"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

const (
	swarmOneOffCleanupTimeout = 30 * time.Second
	maxSwarmServiceNameLength = 63
	// swarmJobLockDir holds one lock file per named Swarm job service
	// (e.g. the shared "image-prune" job every stack deployment can trigger).
	// A plain in-process mutex only serializes callers within a single doco-cd
	// process; "go test ./..." runs each package as its own process, and
	// multiple doco-cd replicas can share one Swarm cluster, so without a
	// cross-process lock two callers can both update the same job service at
	// once. The loser then observes the job iteration advance to the winner's
	// run while it is still waiting on its own, and errors out.
	swarmJobLockDir = "doco-cd-swarm-job-locks"
)

func swarmJobLockPath(name string) string {
	return filepath.Join(os.TempDir(), swarmJobLockDir, name)
}

// RunSwarmJob runs a Docker Swarm job container with the specified mode and command.
// https://docs.docker.com/reference/cli/docker/service/create/#running-as-a-job
func RunSwarmJob(ctx context.Context, dockerCLI command.Cli, mode swarm.DeployMode, command []string, title string, logs ...*slog.Logger) error {
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

	var jobLog *slog.Logger
	if len(logs) > 0 {
		jobLog = logs[0]
	}

	unlock := sourcecache.AcquireExclusivePathLock(swarmJobLockPath(name))
	defer unlock()

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
		logSwarmJob(jobLog, "created swarm job service", name, serviceID)
	} else {
		// Update existing service to trigger a new job run
		if strings.Contains(err.Error(), "already exists") {
			// Get the existing service ID
			filter := make(client.Filters).Add("name", newServiceSpec.Name)

			listResult, listErr := apiClient.ServiceList(ctx, client.ServiceListOptions{Filters: filter})
			if listErr != nil {
				return fmt.Errorf("error listing job service %s: %w", swarm.ServiceIdentity(name, ""), listErr)
			}

			if len(listResult.Items) == 0 {
				return fmt.Errorf("service %s already exists but could not find it", swarm.ServiceIdentity(name, ""))
			}

			for _, service := range listResult.Items {
				if service.Spec.Name == newServiceSpec.Name {
					serviceID = service.ID
					break
				}
			}

			if serviceID == "" {
				return fmt.Errorf("service %s already exists but could not find its ID", swarm.ServiceIdentity(name, ""))
			}

			logSwarmJob(jobLog, "updating swarm job service", name, serviceID)

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
						return fmt.Errorf("error inspecting existing service %s: %w", swarm.ServiceIdentity(name, serviceID), getErr)
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
				logSwarmJobError(jobLog, "failed to update swarm job service", name, serviceID, updateErr)
				return fmt.Errorf("error updating existing service %s: %w", swarm.ServiceIdentity(name, serviceID), updateErr)
			}
		} else {
			logSwarmJobError(jobLog, "failed to create swarm job service", name, "", err)
			return fmt.Errorf("error creating one-off job service %s: %w", swarm.ServiceIdentity(name, ""), err)
		}
	}

	// Wait for the job's current iteration to complete or fail.
	logSwarmJob(jobLog, "waiting for swarm job service", name, serviceID)

	err = swarm.WaitOnJobService(ctx, dockerCLI, serviceID, previousJobIteration)
	if err != nil {
		logSwarmJobError(jobLog, "swarm job service failed", name, serviceID, err)
		return fmt.Errorf("error waiting for job service %s: %w", swarm.ServiceIdentity(name, serviceID), err)
	}

	return nil
}

// RunImagePruneJob runs a Docker Swarm global job to prune unused images on all nodes.
func RunImagePruneJob(ctx context.Context, dockerCLI command.Cli, logs ...*slog.Logger) error {
	return RunSwarmJob(ctx, dockerCLI, swarm.DeployModeGlobalJob, []string{"docker", "image", "prune", "--force"}, "image-prune", logs...)
}

// RunImageRemoveJob runs a Docker Swarm global job to remove specified images.
func RunImageRemoveJob(ctx context.Context, dockerCLI command.Cli, images []string, logs ...*slog.Logger) error {
	args := append([]string{"docker", "image", "rm", "--force"}, images...)
	return RunSwarmJob(ctx, dockerCLI, swarm.DeployModeGlobalJob, args, "image-remove", logs...)
}

type SwarmOneOffFromServiceOptions struct {
	RunID            string
	ScheduledAt      string
	StartedAt        string
	Replicas         uint64
	SendRegistryAuth bool
	KeepService      bool
	Logger           *slog.Logger
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

	var oneOffSpec swarmTypes.ServiceSpec

	clone.Deep(&oneOffSpec, &sourceService.Spec)
	oneOffSpec.Name = swarmOneOffServiceName(sourceService.Spec.Name, time.Now())

	if oneOffSpec.TaskTemplate.ContainerSpec == nil {
		return fmt.Errorf("service %s has no task container spec", serviceName)
	}

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
		if opts.RunID != "" {
			labels[DocoCDJobLabels.JobRunID] = opts.RunID
		}

		if opts.ScheduledAt != "" {
			labels[DocoCDJobLabels.JobScheduledAt] = opts.ScheduledAt
		}

		if opts.StartedAt != "" {
			labels[DocoCDJobLabels.JobStartedAt] = opts.StartedAt
		}
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
		logSwarmJobError(opts.Logger, "failed to create one-off swarm service", oneOffSpec.Name, "", err)

		return fmt.Errorf("create one-off service %s from %s: %w",
			swarm.ServiceIdentity(oneOffSpec.Name, ""), swarm.ServiceIdentity(sourceService.Spec.Name, sourceService.ID), err)
	}

	logSwarmJob(opts.Logger, "created one-off swarm service", oneOffSpec.Name, createResult.ID)

	if !opts.KeepService {
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), swarmOneOffCleanupTimeout)
			defer cancel()

			_, cleanupErr := apiClient.ServiceRemove(cleanupCtx, createResult.ID, client.ServiceRemoveOptions{})
			if cleanupErr == nil || errdefs.IsNotFound(cleanupErr) {
				return
			}

			logSwarmJobError(opts.Logger, "failed to remove one-off swarm service", oneOffSpec.Name, createResult.ID, cleanupErr)

			cleanupErr = fmt.Errorf("remove one-off service %s: %w", swarm.ServiceIdentity(oneOffSpec.Name, createResult.ID), cleanupErr)
			if err == nil {
				err = cleanupErr
			} else {
				err = errors.Join(err, cleanupErr)
			}
		}()
	}

	logSwarmJob(opts.Logger, "waiting for one-off swarm service", oneOffSpec.Name, createResult.ID)

	if err = swarm.WaitOnJobService(ctx, dockerCLI, createResult.ID, nil); err != nil {
		logSwarmJobError(opts.Logger, "one-off swarm service failed", oneOffSpec.Name, createResult.ID, err)
		return fmt.Errorf("wait one-off service %s: %w", swarm.ServiceIdentity(oneOffSpec.Name, createResult.ID), err)
	}

	return nil
}

func swarmOneOffServiceName(sourceServiceName string, now time.Time) string {
	suffix := fmt.Sprintf("%s%d", oneOffServiceNameSeparator, now.UTC().UnixNano())
	maxSourceServiceNameLength := maxSwarmServiceNameLength - len(suffix)

	if len(sourceServiceName) > maxSourceServiceNameLength {
		sourceServiceName = sourceServiceName[:maxSourceServiceNameLength]
	}

	return sourceServiceName + suffix
}

// RemoveSwarmOneOffService removes a retained temporary job service.
func RemoveSwarmOneOffService(ctx context.Context, dockerCLI command.Cli, serviceID string, logs ...*slog.Logger) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), swarmOneOffCleanupTimeout)
	defer cancel()

	var jobLog *slog.Logger
	if len(logs) > 0 {
		jobLog = logs[0]
	}

	name := swarmJobServiceName(cleanupCtx, dockerCLI, serviceID, jobLog)
	logSwarmJob(jobLog, "removing one-off swarm service", name, serviceID)

	_, err := dockerCLI.Client().ServiceRemove(cleanupCtx, serviceID, client.ServiceRemoveOptions{})
	if err != nil && !errdefs.IsNotFound(err) {
		logSwarmJobError(jobLog, "failed to remove one-off swarm service", name, serviceID, err)
		return fmt.Errorf("remove one-off service %s: %w", swarm.ServiceIdentity(name, serviceID), err)
	}

	return nil
}

// FindSwarmOneOffService finds the single retained temporary service for runID.
func FindSwarmOneOffService(ctx context.Context, dockerCLI command.Cli, runID string) (string, error) {
	filter := make(client.Filters)
	filter.Add("label", DocoCDJobLabels.JobRunID+"="+runID)

	result, err := dockerCLI.Client().ServiceList(ctx, client.ServiceListOptions{Filters: filter})
	if err != nil {
		return "", fmt.Errorf("list one-off services for run %s: %w", runID, err)
	}

	if len(result.Items) == 0 {
		return "", nil
	}

	if len(result.Items) != 1 {
		return "", fmt.Errorf("found %d one-off services for run %s", len(result.Items), runID)
	}

	return result.Items[0].ID, nil
}

// WaitOnSwarmOneOffService adopts a retained temporary job service.
func WaitOnSwarmOneOffService(ctx context.Context, dockerCLI command.Cli, serviceID string, logs ...*slog.Logger) error {
	var jobLog *slog.Logger
	if len(logs) > 0 {
		jobLog = logs[0]
	}

	name := swarmJobServiceName(ctx, dockerCLI, serviceID, jobLog)
	logSwarmJob(jobLog, "waiting for one-off swarm service", name, serviceID)

	if err := swarm.WaitOnJobService(ctx, dockerCLI, serviceID, nil); err != nil {
		logSwarmJobError(jobLog, "one-off swarm service failed", name, serviceID, err)
		return fmt.Errorf("wait one-off service %s: %w", swarm.ServiceIdentity(name, serviceID), err)
	}

	return nil
}

func swarmJobServiceName(ctx context.Context, dockerCLI command.Cli, serviceID string, log *slog.Logger) string {
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	result, err := dockerCLI.Client().ServiceInspect(lookupCtx, serviceID, client.ServiceInspectOptions{})
	if err != nil {
		if log != nil {
			log.Debug("could not resolve swarm service name",
				append(swarm.ServiceLogAttrs("", serviceID), slog.Any("error", err))...)
		}

		return swarm.UnavailableIdentity
	}

	return swarm.OrUnavailable(result.Service.Spec.Name)
}

// logSwarmJob records helper-job progress at debug level; failures are logged by logSwarmJobError.
func logSwarmJob(log *slog.Logger, message, name, id string) {
	if log != nil {
		log.Debug(message, swarm.ServiceLogAttrs(name, id)...)
	}
}

func logSwarmJobError(log *slog.Logger, message, name, id string, err error) {
	if log != nil {
		log.Error(message, append(swarm.ServiceLogAttrs(name, id), slog.Any("error", err))...)
	}
}
