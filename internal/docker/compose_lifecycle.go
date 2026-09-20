package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/source/store"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// ErrComposeServiceNotFound indicates that the requested service is not declared by the project.
var ErrComposeServiceNotFound = errors.New("compose service not found")

// ErrComposeSourceRevisionConflict indicates that managed recreation cannot use the deployed source revision.
var ErrComposeSourceRevisionConflict = errors.New("compose source revision conflict")

// DestroyStack destroys the stack using the provided deployment configuration.
func DestroyStack(
	jobLog *slog.Logger, ctx *context.Context,
	dockerCli *command.Cli, deployConfig *deploy.Config, swarmMode bool,
) error {
	stackLog := jobLog.
		With(slog.String("stack", deployConfig.Name))

	stackLog.Info("destroying stack")

	if swarmMode {
		err := RemoveSwarmStack(*ctx, *dockerCli, deployConfig.Name)
		if err != nil {
			errMsg := "failed to destroy swarm stack"
			return fmt.Errorf("%s: %w", errMsg, err)
		}

		return nil
	}

	service, err := compose.NewComposeService(*dockerCli)
	if err != nil {
		return err
	}

	downOpts := api.DownOptions{
		RemoveOrphans: deployConfig.RemoveOrphans,
		Volumes:       deployConfig.Destroy.RemoveVolumes,
	}

	if deployConfig.Destroy.RemoveImages {
		downOpts.Images = "all"
	}

	err = service.Down(*ctx, deployConfig.Name, downOpts)
	if err != nil {
		errMsg := "failed to destroy stack"
		return fmt.Errorf("%s: %w", errMsg, err)
	}

	return nil
}

// RestartProject restarts all services in the specified project.
func RestartProject(ctx context.Context, dockerCli command.Cli, projectName string, timeout time.Duration) error {
	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	return service.Restart(ctx, projectName, api.RestartOptions{
		Timeout: &timeout,
	})
}

// StopProject stops all services in the specified project.
func StopProject(ctx context.Context, dockerCli command.Cli, projectName string, timeout time.Duration) error {
	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	return service.Stop(ctx, projectName, api.StopOptions{
		Timeout: &timeout,
	})
}

// DefaultStopServicesTimeout is the fallback timeout used when stopping a
// target declared via cd.doco.job.stop_services and neither an explicit
// cd.doco.job.stop_services.timeout override nor the target's own configured
// grace period (container StopTimeout / stop_grace_period, or Swarm service
// StopGracePeriod) is available.
const DefaultStopServicesTimeout = 30 * time.Second

// StopProjectServices stops specific named services within a compose project.
// Services are identified by their service name as declared in the compose file
// (the map key under `services:`), not by container_name.
//
// Containers are stopped directly via the Docker API (instead of compose Stop
// with a services filter) to ensure only explicitly targeted services are
// affected, without implicit dependency traversal.
//
// timeoutOverride, when non-nil, is used explicitly for every targeted
// container (this is how cd.doco.job.stop_services.timeout is applied). When
// nil, each container's own configured stop timeout (populated from the
// compose file's stop_grace_period) is honored by leaving the stop request's
// timeout unset, letting the Docker engine apply it; DefaultStopServicesTimeout
// is only used explicitly as a fallback for containers that have no stop
// timeout configured.
//
// This is best-effort: if stopping one container fails, the remaining
// containers are still attempted, and all failures are aggregated into the
// returned error.
func StopProjectServices(ctx context.Context, dockerCli command.Cli, projectName string, services []string, timeoutOverride *time.Duration) error {
	if len(services) == 0 {
		return nil
	}

	serviceSet := set.New(services...)

	containers, err := GetLabeledContainers(ctx, dockerCli.Client(), api.ProjectLabel, projectName, true)
	if err != nil {
		return fmt.Errorf("failed to list containers for project %q: %w", projectName, err)
	}

	var errs []string

	for _, c := range containers {
		svcName := c.Labels[api.ServiceLabel]
		if !serviceSet.Contains(svcName) {
			continue
		}

		if string(c.State) != "running" {
			continue
		}

		stopOpts := client.ContainerStopOptions{}

		switch {
		case timeoutOverride != nil:
			secs := int(timeoutOverride.Seconds())
			stopOpts.Timeout = &secs
		default:
			hasOwnTimeout, inspectErr := containerHasConfiguredStopTimeout(ctx, dockerCli, c.ID)
			if inspectErr != nil {
				errs = append(errs, fmt.Sprintf("container %s (service %q): %v", c.ID[:12], svcName, inspectErr))

				continue
			}

			if !hasOwnTimeout {
				// No container-configured stop timeout: fall back to the
				// default explicitly so behavior for services without a
				// declared stop_grace_period is unchanged.
				secs := int(DefaultStopServicesTimeout.Seconds())
				stopOpts.Timeout = &secs
			}
			// Otherwise leave stopOpts.Timeout nil, so the engine applies
			// the container's own StopTimeout (derived from the compose
			// file's stop_grace_period).
		}

		if _, err := dockerCli.Client().ContainerStop(ctx, c.ID, stopOpts); err != nil {
			errs = append(errs, fmt.Sprintf("container %s (service %q): %v", c.ID[:12], svcName, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to stop %d container(s): %s", len(errs), strings.Join(errs, "; "))
	}

	return nil
}

// containerHasConfiguredStopTimeout reports whether the container has an
// explicit StopTimeout configured (i.e. the compose file declared a stop_grace_period for its service).
// When true, callers should leave the stop request's timeout unset so the Docker engine applies
// the container's own value instead of a hardcoded default.
func containerHasConfiguredStopTimeout(ctx context.Context, dockerCli command.Cli, containerID string) (bool, error) {
	inspectResult, err := dockerCli.Client().ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return false, fmt.Errorf("inspect container %s: %w", containerID, err)
	}

	return inspectResult.Container.Config != nil && inspectResult.Container.Config.StopTimeout != nil, nil
}

// StartProjectServices starts specific named services within a compose project.
// Services are identified by their service name as declared in the compose file
// (the map key under `services:`), not by container_name.
//
// Note: the compose Start API ignores StartOptions.Services, so containers are
// looked up directly by the com.docker.compose.project and com.docker.compose.service
// labels and started individually.
//
// This is best-effort: if starting one container fails, the remaining
// containers are still attempted, and all failures are aggregated into the
// returned error.
func StartProjectServices(ctx context.Context, dockerCli command.Cli, projectName string, services []string) error {
	if len(services) == 0 {
		return nil
	}

	serviceSet := set.New(services...)

	containers, err := GetLabeledContainers(ctx, dockerCli.Client(), api.ProjectLabel, projectName, true)
	if err != nil {
		return fmt.Errorf("failed to list containers for project %q: %w", projectName, err)
	}

	var errs []string

	for _, c := range containers {
		svcName := c.Labels[api.ServiceLabel]
		if !serviceSet.Contains(svcName) {
			continue
		}

		if _, err := dockerCli.Client().ContainerStart(ctx, c.ID, client.ContainerStartOptions{}); err != nil {
			errs = append(errs, fmt.Sprintf("container %s (service %q): %v", c.ID[:12], svcName, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to start %d container(s): %s", len(errs), strings.Join(errs, "; "))
	}

	return nil
}

// StartProject starts all services in the specified project.
func StartProject(ctx context.Context, dockerCli command.Cli, projectName string, timeout time.Duration) error {
	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	return service.Start(ctx, projectName, api.StartOptions{
		Wait:        true,
		WaitTimeout: timeout,
	})
}

// RemoveProject removes the entire project including containers, networks, volumes and images.
func RemoveProject(ctx context.Context, dockerCli command.Cli, projectName string, timeout time.Duration, removeVolumes, removeImages bool) error {
	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	return service.Down(ctx, projectName, projectDownOptions(timeout, removeVolumes, removeImages))
}

func projectDownOptions(timeout time.Duration, removeVolumes, removeImages bool) api.DownOptions {
	images := ""
	if removeImages {
		images = "all"
	}

	return api.DownOptions{
		RemoveOrphans: true,
		Timeout:       &timeout,
		Volumes:       removeVolumes,
		Images:        images,
	}
}

// GetProjects returns a list of all projects.
func GetProjects(ctx context.Context, dockerCli command.Cli, showDisabled bool) ([]api.Stack, error) {
	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return nil, err
	}

	return service.List(ctx, api.ListOptions{
		All: showDisabled,
	})
}

// RecreateProject recreates services in the specified project.
// If serviceName is empty, all services are recreated.
// If serviceName is specified, only that service is recreated.
func RecreateProject(
	ctx context.Context,
	contextName string,
	dockerCli command.Cli,
	projectName string,
	serviceName string,
	timeout time.Duration,
	secretProvider secretprovider.SecretProvider,
	opts ScheduledComposeOptions,
) error {
	lock.LockStack(lock.StackKey(contextName, projectName))
	defer lock.UnlockStack(lock.StackKey(contextName, projectName))

	containers, err := GetProjectContainers(ctx, dockerCli, projectName)
	if err != nil {
		return fmt.Errorf("failed to get project containers: %w", err)
	}

	if len(containers) == 0 {
		return fmt.Errorf("project not found or has no containers: %s", projectName)
	}

	labels := recreateProjectLabels(containers)

	ref, err := composeScheduledServiceRefFromLabels(labels)
	if err != nil {
		return fmt.Errorf("parse project metadata: %w", err)
	}

	if ref.DeploymentName != "" && ref.RepositoryURL != "" {
		return recreateManagedProject(ctx, dockerCli, ref, labels, serviceName, timeout, secretProvider, opts)
	}

	return recreateStandardProject(ctx, dockerCli, ref, labels, containers, serviceName, timeout, opts.ComposeLoad)
}

// recreateProjectLabels selects metadata from the managed container when available.
func recreateProjectLabels(containers []api.ContainerSummary) map[string]string {
	var fallback map[string]string

	for _, container := range containers {
		labels := container.Labels
		if fallback == nil && strings.TrimSpace(labels[api.WorkingDirLabel]) != "" {
			fallback = labels
		}

		if strings.TrimSpace(labels[DocoCDLabels.Deployment.Name]) != "" &&
			(strings.TrimSpace(labels[DocoCDLabels.Source.URL]) != "" ||
				strings.TrimSpace(labels[DocoCDLabels.Source.Name]) != "") {
			return labels
		}
	}

	return fallback
}

// recreateManagedProject reloads and force-recreates a project deployed by doco-cd.
func recreateManagedProject(
	ctx context.Context,
	dockerCli command.Cli,
	ref composeScheduledServiceRef,
	labels map[string]string,
	serviceName string,
	timeout time.Duration,
	secretProvider secretprovider.SecretProvider,
	opts ScheduledComposeOptions,
) error {
	sourceRepoPath, sourceType, err := resolveScheduledSourceRepo(ref, opts.ComposeLoad.DataMountPath)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve cached source for project %s: %v",
			ErrComposeSourceRevisionConflict, ref.Project, err)
	}

	unlockSource, err := lockScheduledSource(ref, opts.ComposeLoad.DataMountPath, sourceRepoPath)
	if err != nil {
		return fmt.Errorf("%w: lock cached source for project %s: %v",
			ErrComposeSourceRevisionConflict, ref.Project, err)
	}
	defer unlockSource()

	if err := validateManagedRecreateRevision(ref, labels, sourceRepoPath, sourceType); err != nil {
		return err
	}

	project, deployConfig, err := loadComposeScheduledProjectAll(ctx, dockerCli, ref, secretProvider, opts)
	if err != nil {
		return fmt.Errorf("reload managed compose project %s: %w", ref.Project, err)
	}

	restoreDeploymentConfigHash(deployConfig, labels)

	project, services, err := selectRecreateServices(project, serviceName, nil)
	if err != nil {
		return err
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)
	sourceURL := strings.TrimSpace(labels[DocoCDLabels.Source.URL])
	payload := &webhook.ParsedPayload{
		Source:   webhook.PayloadSource(SourceTypeLabelValue(string(sourceType), labels[DocoCDLabels.Source.Type])),
		Trigger:  "api.recreate",
		FullName: strings.TrimSpace(labels[DocoCDLabels.Source.Name]),
		WebURL:   sourceURL,
	}

	addComposeServiceLabels(
		project,
		deployConfig,
		payload,
		sourceURL,
		ref.WorkingDir,
		app.Version,
		timestamp,
		ComposeVersion,
		strings.TrimSpace(labels[DocoCDLabels.Deployment.CommitSHA]),
		strings.TrimSpace(labels[DocoCDLabels.Deployment.ComposeHash]),
	)
	addComposeVolumeLabels(
		project,
		deployConfig,
		payload,
		sourceURL,
		app.Version,
		timestamp,
		ComposeVersion,
		strings.TrimSpace(labels[DocoCDLabels.Deployment.CommitSHA]),
		strings.TrimSpace(labels[DocoCDLabels.Deployment.ComposeHash]),
	)

	recreateConfig := *deployConfig
	recreateConfig.Timeout = int(timeout.Seconds())

	if err := deployCompose(ctx, dockerCli, project, &recreateConfig, api.RecreateForce, services, nil, func(string) {}); err != nil {
		return fmt.Errorf("recreate managed compose project %s: %w", ref.Project, err)
	}

	return nil
}

// validateManagedRecreateRevision ensures the cache still contains the deployed revision.
func validateManagedRecreateRevision(
	ref composeScheduledServiceRef,
	labels map[string]string,
	sourceRepoPath string,
	sourceType config.SourceType,
) error {
	expected := strings.TrimSpace(labels[DocoCDLabels.Deployment.CommitSHA])
	if expected == "" {
		return fmt.Errorf("%w: project %s has no deployed revision; run a normal deployment before recreating",
			ErrComposeSourceRevisionConflict, ref.Project)
	}

	// sourceRepoPath is a store base directory (holds mirror/ or the OCI pull cache, plus artifacts/<revision>),
	// not a checked-out working tree - the expected revision is still cached only if it was published as its own artifact.
	switch sourceType {
	case config.SourceTypeGit:
		gitStore, err := store.NewGitStore(store.GitStoreOptions{CloneURL: ref.RepositoryURL, BaseDir: sourceRepoPath})
		if err != nil {
			return fmt.Errorf("%w: cannot verify cached Git source for project %s: %v",
				ErrComposeSourceRevisionConflict, ref.Project, err)
		}

		if _, matches, err := gitStore.Lookup(store.Revision(expected)); err != nil {
			return fmt.Errorf("%w: cannot verify cached Git source for project %s: %v",
				ErrComposeSourceRevisionConflict, ref.Project, err)
		} else if matches {
			return nil
		}
	case config.SourceTypeOCI:
		ociStore, err := store.NewOCIStore(store.OCIStoreOptions{ArtifactRef: ref.RepositoryURL, BaseDir: sourceRepoPath})
		if err != nil {
			return fmt.Errorf("%w: cannot verify cached OCI source for project %s: %v",
				ErrComposeSourceRevisionConflict, ref.Project, err)
		}

		if _, matches, err := ociStore.Lookup(store.Revision(expected)); err != nil {
			return fmt.Errorf("%w: cannot verify cached OCI source for project %s: %v",
				ErrComposeSourceRevisionConflict, ref.Project, err)
		} else if matches {
			return nil
		}
	}

	return fmt.Errorf("%w: cached %s source for project %s does not match deployed revision %s; run a normal deployment before recreating",
		ErrComposeSourceRevisionConflict, sourceType, ref.Project, expected)
}

// restoreDeploymentConfigHash retains the deployed hash when recreating a managed project.
func restoreDeploymentConfigHash(deployConfig *deploy.Config, labels map[string]string) {
	if configHash := strings.TrimSpace(labels[DocoCDLabels.Deployment.ConfigHash]); configHash != "" {
		deployConfig.Internal.Hash = configHash
	}
}

// recreateStandardProject force-recreates a Compose project using its container metadata.
func recreateStandardProject(
	ctx context.Context,
	dockerCli command.Cli,
	ref composeScheduledServiceRef,
	labels map[string]string,
	containers []api.ContainerSummary,
	serviceName string,
	timeout time.Duration,
	loadOpts ComposeLoadOptions,
) error {
	if len(ref.ConfigFiles) == 0 {
		return fmt.Errorf("%w: missing %q label", ErrComposeScheduledMetadataUnavailable, api.ConfigFilesLabel)
	}

	project, err := LoadCompose(
		ctx,
		dockerCli,
		ref.WorkingDir,
		ref.WorkingDir,
		ref.Project,
		ref.ConfigFiles,
		splitCommaSeparatedLabelValues(labels[api.EnvironmentFileLabel]),
		[]string{"*"},
		nil,
		loadOpts,
	)
	if err != nil {
		return fmt.Errorf("load compose project %s: %w", ref.Project, err)
	}

	activeServices := make([]string, 0, len(containers))
	for _, container := range containers {
		service := strings.TrimSpace(container.Labels[api.ServiceLabel])
		if _, declared := project.Services[service]; service != "" && declared {
			activeServices = append(activeServices, service)
		}
	}

	project, services, err := selectRecreateServices(project, serviceName, activeServices)
	if err != nil {
		return err
	}

	addComposeServiceTrackingLabels(project)

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return fmt.Errorf("create compose service: %w", err)
	}

	if err := service.Up(ctx, project, api.UpOptions{
		Create: api.CreateOptions{
			Services:             services,
			RemoveOrphans:        true,
			Recreate:             api.RecreateForce,
			RecreateDependencies: api.RecreateDiverged,
			Timeout:              &timeout,
		},
		Start: api.StartOptions{
			Project: project,
		},
	}); err != nil {
		return fmt.Errorf("recreate compose project %s: %w", ref.Project, err)
	}

	return nil
}

// selectRecreateServices returns the requested service or the currently active project services.
func selectRecreateServices(project *types.Project, serviceName string, activeServices []string) (*types.Project, []string, error) {
	if serviceName != "" {
		if _, ok := project.Services[serviceName]; !ok {
			return nil, nil, fmt.Errorf("%w: %s/%s", ErrComposeServiceNotFound, project.Name, serviceName)
		}

		selected, err := project.WithSelectedServices([]string{serviceName}, types.IncludeDependencies)
		if err != nil {
			return nil, nil, fmt.Errorf("select compose service %s/%s: %w", project.Name, serviceName, err)
		}

		return selected, []string{serviceName}, nil
	}

	if len(activeServices) == 0 {
		return project, nil, nil
	}

	selected, err := project.WithSelectedServices(activeServices, types.IncludeDependencies)
	if err != nil {
		return nil, nil, fmt.Errorf("select active services for compose project %s: %w", project.Name, err)
	}

	return selected, nil, nil
}

// GetProjectContainers returns the status of all services in the specified project.
func GetProjectContainers(ctx context.Context, dockerCli command.Cli, projectName string) ([]api.ContainerSummary, error) {
	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return nil, err
	}

	return service.Ps(ctx, projectName, api.PsOptions{
		All: true,
	})
}
