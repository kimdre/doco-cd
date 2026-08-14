package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	containerTypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

var (
	ErrComposeScheduledMetadataUnavailable = errors.New("compose scheduled-job metadata unavailable")
	ErrComposeScheduledServiceReplicated   = errors.New("standalone scheduled compose service must have exactly one replica")
)

type composeScheduledServiceRef struct {
	Project        string
	Service        string
	WorkingDir     string
	ConfigFiles    []string
	RepositoryURL  string
	DeploymentName string
	ConfigTarget   string
	Reference      string
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

	project, err = prepareComposeProjectForOneOffRun(project, ref.Service)
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

// prepareComposeProjectForOneOffRun returns a copy of project whose target
// service is marked as a scheduler-created ephemeral run. This keeps Compose
// one-off containers from being rediscovered as standalone scheduled jobs while
// preserving the rest of the service definition used to launch them.
func prepareComposeProjectForOneOffRun(project *types.Project, serviceName string) (*types.Project, error) {
	if project == nil {
		return nil, errors.New("compose project is required")
	}

	svc, ok := project.Services[serviceName]
	if !ok {
		return nil, fmt.Errorf("compose service %q not found", serviceName)
	}

	if svc.Labels == nil {
		svc.Labels = map[string]string{}
	} else {
		svc.Labels = maps.Clone(svc.Labels)
	}

	if svc.CustomLabels == nil {
		svc.CustomLabels = map[string]string{}
	} else {
		svc.CustomLabels = maps.Clone(svc.CustomLabels)
	}

	// Ensure the one-off container is created with the standard Compose tracking
	// labels so it remains attributable to its compose project/service in
	// downstream systems such as log aggregation.
	svc.CustomLabels[api.ProjectLabel] = project.Name
	svc.CustomLabels[api.ServiceLabel] = svc.Name
	svc.CustomLabels[api.WorkingDirLabel] = project.WorkingDir
	svc.CustomLabels[api.ConfigFilesLabel] = strings.Join(project.ComposeFiles, ",")
	svc.CustomLabels[api.VersionLabel] = api.ComposeVersion

	// Set oneoff=False on the service definition (Compose will overwrite it to True
	// on the actual created container). We do not set it to True here because
	// com.docker.compose.oneoff is a runtime marker managed by Compose itself—setting
	// it on the service definition would blur the semantics and risk side effects.
	// The actual container created by RunOneOffContainer will get oneoff=True,
	// which we rely on as a fallback ephemeral detection mechanism in the scheduler.
	svc.CustomLabels[api.OneoffLabel] = "False"

	svc.Labels[DocoCDJobLabels.JobEphemeral] = "true"
	svc.CustomLabels[DocoCDJobLabels.JobEphemeral] = "true"

	projectCopy := *project
	projectCopy.Services = maps.Clone(project.Services)
	projectCopy.Services[serviceName] = svc

	return &projectCopy, nil
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

	deployConfig, repoPath, err := loadComposeScheduledDeployConfig(ctx, ref, secretProvider)
	if err != nil {
		return nil, err
	}

	project, err := LoadCompose(ctx, dockerCli, repoPath, ref.WorkingDir, ref.Project, ref.ConfigFiles,
		deployConfig.EnvFiles, deployConfig.Profiles, deployConfig.Internal.Environment)
	if err != nil {
		return nil, fmt.Errorf("load compose project for scheduled service %s/%s: %w", ref.Project, ref.Service, err)
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

	// Prefer the full source URL label to reconstruct a host-qualified
	// repository path (e.g. "github.com/owner/repo") via git.GetRepoName().
	// The source "name" label only holds the short "owner/repo" form and
	// cannot be used for this, since it does not carry the host segment.
	repositoryURL := strings.TrimSpace(labels[DocoCDLabels.Source.URL])
	if repositoryURL == "" {
		repositoryURL = strings.TrimSpace(labels[DocoCDLabels.Source.Name])
	}

	ref := composeScheduledServiceRef{
		Project:        project,
		Service:        service,
		WorkingDir:     strings.TrimSpace(labels[api.WorkingDirLabel]),
		ConfigFiles:    splitCommaSeparatedLabelValues(labels[api.ConfigFilesLabel]),
		RepositoryURL:  repositoryURL,
		DeploymentName: strings.TrimSpace(labels[DocoCDLabels.Deployment.Name]),
		ConfigTarget:   strings.TrimSpace(labels[DocoCDLabels.Deployment.ConfigTarget]),
		Reference:      strings.TrimSpace(labels[DocoCDLabels.Deployment.TargetRef]),
	}

	return ref, nil
}

// loadComposeScheduledDeployConfig reloads the deploy config that originally
// defined the scheduled Compose service and returns the repository root that
// should be used for LoadCompose interpolation and relative path resolution.
func loadComposeScheduledDeployConfig(
	ctx context.Context,
	ref composeScheduledServiceRef,
	secretProvider *secretprovider.SecretProvider,
) (*deploy.Config, string, error) {
	if strings.TrimSpace(ref.RepositoryURL) == "" || strings.TrimSpace(ref.DeploymentName) == "" {
		return nil, "", fmt.Errorf("%w: missing deployment repository and/or name label",
			ErrComposeScheduledMetadataUnavailable)
	}

	appConfig, err := app.GetConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load app config for scheduled service %s/%s: %w", ref.Project, ref.Service, err)
	}

	sourceRepoPath, err := filesystem.VerifyAndSanitizePath(
		filepath.Join(appConfig.DataMountPath, git.GetRepoName(ref.RepositoryURL)),
		appConfig.DataMountPath,
	)
	if err != nil {
		return nil, "", fmt.Errorf("resolve source repository path for scheduled service %s/%s: %w", ref.Project, ref.Service, err)
	}

	repoPath, err := resolveScheduledComposeRepoRoot(ref.WorkingDir, appConfig.DataMountPath, sourceRepoPath)
	if err != nil {
		return nil, "", err
	}

	configs, err := deploy.GetConfigs(sourceRepoPath, appConfig.DeployConfigBaseDir, ref.ConfigTarget, ref.Reference, nil)
	if err != nil {
		return nil, "", fmt.Errorf("load deploy config for scheduled service %s/%s: %w", ref.Project, ref.Service, err)
	}

	deployConfig, err := findComposeScheduledDeployConfig(configs, ref.DeploymentName)
	if err != nil {
		return nil, "", err
	}

	if err = prepareComposeScheduledDeployConfig(ctx, deployConfig, sourceRepoPath, repoPath, secretProvider); err != nil {
		return nil, "", err
	}

	return deployConfig, repoPath, nil
}

// findComposeScheduledDeployConfig selects the deploy config matching the
// scheduled service's deployment name from the reloaded config set.
func findComposeScheduledDeployConfig(configs []*deploy.Config, deploymentName string) (*deploy.Config, error) {
	for _, cfg := range configs {
		if strings.TrimSpace(cfg.Name) == deploymentName {
			return cfg, nil
		}
	}

	return nil, fmt.Errorf("load deploy config for scheduled service: %w: deployment %q not found",
		ErrComposeScheduledMetadataUnavailable, deploymentName)
}

// prepareComposeScheduledDeployConfig rebuilds the interpolation environment
// used by LoadCompose from env files, deploy-config environment, and any
// external secrets that resolve to env vars at run time.
func prepareComposeScheduledDeployConfig(
	ctx context.Context,
	deployConfig *deploy.Config,
	sourceRepoPath string,
	repoPath string,
	secretProvider *secretprovider.SecretProvider,
) error {
	if deployConfig == nil {
		return fmt.Errorf("%w: missing deployment config", ErrComposeScheduledMetadataUnavailable)
	}

	if deployConfig.RepositoryUrl != "" {
		if err := deploy.LoadLocalDotEnv(deployConfig, sourceRepoPath); err != nil {
			return fmt.Errorf("load local env files for scheduled service %s: %w", deployConfig.Name, err)
		}

		if err := deploy.LoadLocalDotEnv(deployConfig, filepath.Join(repoPath, deployConfig.WorkingDirectory)); err != nil {
			return fmt.Errorf("load remote env files for scheduled service %s: %w", deployConfig.Name, err)
		}
	} else {
		if err := deploy.LoadLocalDotEnv(deployConfig, filepath.Join(sourceRepoPath, deployConfig.WorkingDirectory)); err != nil {
			return fmt.Errorf("load env files for scheduled service %s: %w", deployConfig.Name, err)
		}
	}

	if deployConfig.Internal.Environment == nil {
		deployConfig.Internal.Environment = make(map[string]string)
	}

	maps.Copy(deployConfig.Internal.Environment, deployConfig.Environment)

	if secretProvider == nil || *secretProvider == nil || len(deployConfig.ExternalSecrets) == 0 {
		return nil
	}

	encodedSecrets, err := secrettypes.EncodeExternalSecretRefs(deployConfig.ExternalSecrets)
	if err != nil {
		return fmt.Errorf("encode external secrets for scheduled service %s: %w", deployConfig.Name, err)
	}

	resolvedSecrets, err := (*secretProvider).ResolveSecretReferences(ctx, encodedSecrets)
	if err != nil {
		return fmt.Errorf("resolve external secrets for scheduled service %s: %w", deployConfig.Name, err)
	}

	maps.Copy(deployConfig.Internal.Environment, resolvedSecrets)

	return nil
}

// resolveScheduledComposeRepoRoot walks up from the scheduled service working
// directory to recover the checked-out repository root under the data mount.
// If no git root can be found, it falls back to the repository path derived
// from the deployment source label.
func resolveScheduledComposeRepoRoot(workingDir, dataMountPath, fallbackRepoPath string) (string, error) {
	resolvedWorkingDir, err := filepath.EvalSymlinks(workingDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolve scheduled compose working directory %q: %w", workingDir, err)
		}

		resolvedWorkingDir = filepath.Clean(workingDir)
	}

	for dir := resolvedWorkingDir; filesystem.InBasePath(dataMountPath, dir); dir = filepath.Dir(dir) {
		if filesystem.IsDir(filepath.Join(dir, ".git")) {
			return dir, nil
		}

		if dir == filepath.Clean(dataMountPath) {
			break
		}

		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}

	if fallbackRepoPath != "" {
		return fallbackRepoPath, nil
	}

	return "", fmt.Errorf("%w: could not determine repository root for %q",
		ErrComposeScheduledMetadataUnavailable, workingDir)
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
