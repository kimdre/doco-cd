package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/deploy"
)

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

// StopProjectServices stops specific named services within a compose project.
// Services are identified by their service name as declared in the compose file
// (the map key under `services:`), not by container_name.
//
// Containers are stopped directly via the Docker API (instead of compose Stop
// with a services filter) to ensure only explicitly targeted services are
// affected, without implicit dependency traversal.
//
// This is best-effort: if stopping one container fails, the remaining
// containers are still attempted, and all failures are aggregated into the
// returned error.
func StopProjectServices(ctx context.Context, dockerCli command.Cli, projectName string, services []string, timeout time.Duration) error {
	if len(services) == 0 {
		return nil
	}

	serviceSet := make(map[string]struct{}, len(services))
	for _, s := range services {
		serviceSet[s] = struct{}{}
	}

	containers, err := GetLabeledContainers(ctx, dockerCli.Client(), api.ProjectLabel, projectName, true)
	if err != nil {
		return fmt.Errorf("failed to list containers for project %q: %w", projectName, err)
	}

	timeoutSecs := int(timeout.Seconds())

	stopOpts := client.ContainerStopOptions{}
	if timeoutSecs > 0 {
		stopOpts.Timeout = &timeoutSecs
	}

	var errs []string

	for _, c := range containers {
		svcName := c.Labels[api.ServiceLabel]
		if _, ok := serviceSet[svcName]; !ok {
			continue
		}

		if string(c.State) != "running" {
			continue
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

	serviceSet := make(map[string]struct{}, len(services))
	for _, s := range services {
		serviceSet[s] = struct{}{}
	}

	containers, err := GetLabeledContainers(ctx, dockerCli.Client(), api.ProjectLabel, projectName, true)
	if err != nil {
		return fmt.Errorf("failed to list containers for project %q: %w", projectName, err)
	}

	var errs []string

	for _, c := range containers {
		svcName := c.Labels[api.ServiceLabel]
		if _, ok := serviceSet[svcName]; !ok {
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
