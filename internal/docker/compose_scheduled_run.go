package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	composeCli "github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	containerTypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/secretprovider"
)

var (
	ErrComposeScheduledMetadataUnavailable = errors.New("compose scheduled-job metadata unavailable")
	ErrComposeScheduledServiceReplicated   = errors.New("standalone scheduled compose service must have exactly one replica")
)

type composeScheduledServiceRef struct {
	Project                string
	Service                string
	WorkingDir             string
	ConfigFiles            []string
	EncodedExternalSecrets map[string]string // provider-ready encoded external secret refs from deploy time
}

func RunComposeScheduledContainer(
	ctx context.Context,
	dockerCli command.Cli,
	containerID string,
	labels map[string]string,
	waitForExit bool,
	secretProvider *secretprovider.SecretProvider,
) error {
	ref, err := composeScheduledServiceRefFromLabels(labels)
	if err != nil {
		return err
	}

	project, err := loadComposeScheduledProject(ctx, dockerCli, ref, secretProvider)
	if err != nil {
		return err
	}

	if err := validateComposeScheduledServiceScale(project, ref); err != nil {
		return err
	}

	inspectResult, err := dockerCli.Client().ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect container %s: %w", containerID, err)
	}

	// Keep prior scheduler behavior for running containers:
	// restart when already running, start when stopped.
	if inspectResult.Container.State != nil && inspectResult.Container.State.Running {
		if waitForExit {
			return RestartContainerAndWait(ctx, dockerCli.Client(), containerID)
		}

		return RestartContainer(ctx, dockerCli.Client(), containerID)
	}

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return fmt.Errorf("create compose service: %w", err)
	}

	var waitResult client.ContainerWaitResult
	if waitForExit {
		waitResult = dockerCli.Client().ContainerWait(ctx, containerID, client.ContainerWaitOptions{
			Condition: containerTypes.WaitConditionNextExit,
		})
	}

	if err := service.Start(ctx, ref.Project, api.StartOptions{
		Project:  project,
		Services: []string{ref.Service},
	}); err != nil {
		return fmt.Errorf("start compose service %s/%s: %w", ref.Project, ref.Service, err)
	}

	if waitForExit {
		return awaitContainerExit(waitResult, containerID)
	}

	return nil
}

func RunComposeOneOffFromServiceDefinition(
	ctx context.Context,
	dockerCli command.Cli,
	labels map[string]string,
	secretProvider *secretprovider.SecretProvider,
) error {
	ref, err := composeScheduledServiceRefFromLabels(labels)
	if err != nil {
		return err
	}

	project, err := loadComposeScheduledProject(ctx, dockerCli, ref, secretProvider)
	if err != nil {
		return err
	}

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return fmt.Errorf("create compose service: %w", err)
	}

	exitCode, err := service.RunOneOffContainer(ctx, project, api.RunOptions{
		Service:     ref.Service,
		NoDeps:      true,
		AutoRemove:  true,
		Tty:         false,
		Interactive: false,
	})
	if err != nil {
		return fmt.Errorf("run compose one-off service %s/%s: %w", ref.Project, ref.Service, err)
	}

	if exitCode != 0 {
		return &ContainerExitError{
			ContainerID: ref.Project + "/" + ref.Service,
			ExitCode:    exitCode,
		}
	}

	return nil
}

func loadComposeScheduledProject(
	ctx context.Context,
	dockerCli command.Cli,
	ref composeScheduledServiceRef,
	secretProvider *secretprovider.SecretProvider,
) (*types.Project, error) {
	if ref.WorkingDir == "" || len(ref.ConfigFiles) == 0 {
		return nil, fmt.Errorf("%w: missing %q and/or %q label",
			ErrComposeScheduledMetadataUnavailable,
			api.WorkingDirLabel,
			api.ConfigFilesLabel,
		)
	}

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return nil, fmt.Errorf("create compose service: %w", err)
	}

	project, err := service.LoadProject(ctx, api.ProjectLoadOptions{
		ProjectName: ref.Project,
		ConfigPaths: ref.ConfigFiles,
		WorkingDir:  ref.WorkingDir,
		Services:    []string{ref.Service},
		All:         true,
		ProjectOptionsFns: []composeCli.ProjectOptionsFn{
			composeCli.WithResolvedPaths(true),
			composeCli.WithInterpolation(true),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("load compose project for scheduled service %s/%s: %w", ref.Project, ref.Service, err)
	}

	// Re-resolve external secrets so that environment-backed compose secrets
	// (secrets.my_secret.environment: MY_VAR) have the correct value injected
	// when compose calls injectSecrets during Start/RunOneOffContainer.
	if secretProvider != nil && *secretProvider != nil && len(ref.EncodedExternalSecrets) > 0 {
		resolved, resolveErr := (*secretProvider).ResolveSecretReferences(ctx, ref.EncodedExternalSecrets)
		if resolveErr != nil {
			return nil, fmt.Errorf("re-resolve external secrets for scheduled service %s/%s: %w", ref.Project, ref.Service, resolveErr)
		}

		if project.Environment == nil {
			project.Environment = make(map[string]string)
		}

		maps.Copy(project.Environment, resolved)
	}

	project, err = project.WithSelectedServices([]string{ref.Service}, types.IgnoreDependencies)
	if err != nil {
		return nil, fmt.Errorf("select compose service %s/%s: %w", ref.Project, ref.Service, err)
	}

	return project, nil
}

func validateComposeScheduledServiceScale(project *types.Project, ref composeScheduledServiceRef) error {
	scheduledService, err := project.GetService(ref.Service)
	if err != nil {
		return fmt.Errorf("get compose service %s/%s: %w", ref.Project, ref.Service, err)
	}

	if scheduledService.GetScale() != 1 {
		return fmt.Errorf("%w: service %s/%s has scale %d",
			ErrComposeScheduledServiceReplicated,
			ref.Project,
			ref.Service,
			scheduledService.GetScale(),
		)
	}

	return nil
}

func composeScheduledServiceRefFromLabels(labels map[string]string) (composeScheduledServiceRef, error) {
	if labels == nil {
		return composeScheduledServiceRef{}, fmt.Errorf("%w: missing labels", ErrComposeScheduledMetadataUnavailable)
	}

	project := strings.TrimSpace(labels[api.ProjectLabel])
	service := strings.TrimSpace(labels[api.ServiceLabel])

	if project == "" || service == "" {
		return composeScheduledServiceRef{}, fmt.Errorf("%w: missing %q and/or %q label",
			ErrComposeScheduledMetadataUnavailable,
			api.ProjectLabel,
			api.ServiceLabel,
		)
	}

	ref := composeScheduledServiceRef{
		Project:     project,
		Service:     service,
		WorkingDir:  strings.TrimSpace(labels[api.WorkingDirLabel]),
		ConfigFiles: splitCommaSeparatedLabelValues(labels[api.ConfigFilesLabel]),
	}

	if raw := strings.TrimSpace(labels[DocoCDJobLabels.JobExternalRefs]); raw != "" {
		var encodedRefs map[string]string
		if err := json.Unmarshal([]byte(raw), &encodedRefs); err == nil {
			ref.EncodedExternalSecrets = encodedRefs
		}
	}

	return ref, nil
}

func splitCommaSeparatedLabelValues(raw string) []string {
	values := []string{}

	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		values = append(values, entry)
	}

	return values
}
