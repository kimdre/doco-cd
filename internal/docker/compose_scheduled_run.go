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

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
	"github.com/kimdre/doco-cd/internal/source/oci"
)

var (
	ErrComposeScheduledMetadataUnavailable = errors.New("compose scheduled-job metadata unavailable")
	ErrComposeScheduledServiceReplicated   = errors.New("standalone scheduled compose service must have exactly one replica")
	// ErrComposeScheduledSourceUnavailable reports that a deployment's Git checkout or extracted
	// OCI artifact is not (yet) present under the data mount, so its deploy config cannot be
	// reloaded. It is transient right after startup, before the first poll has fetched the
	// source, which is why callers that run on a timer treat it as "retry later" rather than as a
	// deployment failure.
	ErrComposeScheduledSourceUnavailable = errors.New("deployment source not available on disk")
)

type composeScheduledServiceRef struct {
	Project        string
	Service        string
	WorkingDir     string
	ConfigFiles    []string
	RepositoryURL  string
	SourceType     string
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
	secretProvider secretprovider.SecretProvider,
	opts ScheduledComposeOptions,
) error {
	ref, err := composeScheduledServiceRefFromLabels(labels)
	if err != nil {
		return err
	}

	project, err := loadComposeScheduledProject(ctx, dockerCli, ref, secretProvider, opts)
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
	secretProvider secretprovider.SecretProvider,
	opts ScheduledComposeOptions,
) error {
	ref, err := composeScheduledServiceRefFromLabels(labels)
	if err != nil {
		return err
	}

	project, err := loadComposeScheduledProject(ctx, dockerCli, ref, secretProvider, opts)
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
	// com.docker.compose.oneoff is a runtime marker managed by Compose itself. Setting
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
	secretProvider secretprovider.SecretProvider,
	opts ScheduledComposeOptions,
) (*types.Project, error) {
	project, _, err := loadComposeScheduledProjectAll(ctx, dockerCli, ref, secretProvider, opts)
	if err != nil {
		return nil, err
	}

	project, err = project.WithSelectedServices([]string{ref.Service}, types.IgnoreDependencies)
	if err != nil {
		return nil, fmt.Errorf("select compose service %s/%s: %w", ref.Project, ref.Service, err)
	}

	return project, nil
}

// loadComposeScheduledProjectAll reloads the deploy config referenced by ref and builds the full
// compose project (all services, not just ref.Service) with freshly re-resolved external secrets.
// It also returns the reloaded deploy config, which callers may need for the actual deploy/apply step.
//
// This is shared by the scheduled-job runner (which then selects a single service) and certificate
// rotation (which needs every service in the project reloaded to identify those consuming the
// freshly issued certificates).
func loadComposeScheduledProjectAll(
	ctx context.Context,
	dockerCli command.Cli,
	ref composeScheduledServiceRef,
	secretProvider secretprovider.SecretProvider,
	opts ScheduledComposeOptions,
) (*types.Project, *deploy.Config, error) {
	if ref.WorkingDir == "" {
		return nil, nil, fmt.Errorf("%w: missing %q label",
			ErrComposeScheduledMetadataUnavailable,
			api.WorkingDirLabel,
		)
	}

	deployConfig, repoPath, err := loadComposeScheduledDeployConfig(ctx, ref, secretProvider, opts)
	if err != nil {
		return nil, nil, err
	}

	// Fall back to the reloaded deploy config's compose files if the label is empty
	// (Swarm) or stale (e.g. renamed compose file not yet reflected in the label).
	configFiles := ref.ConfigFiles
	if len(configFiles) == 0 || !composeConfigFilesExist(configFiles, ref.WorkingDir) {
		configFiles = deployConfig.ComposeFiles
	}

	if len(configFiles) == 0 {
		return nil, nil, fmt.Errorf("%w: missing %q label and no compose files configured",
			ErrComposeScheduledMetadataUnavailable,
			api.ConfigFilesLabel,
		)
	}

	project, err := LoadCompose(ctx, dockerCli, repoPath, ref.WorkingDir, ref.Project, configFiles,
		deployConfig.EnvFiles, deployConfig.Profiles, deployConfig.Internal.Environment, opts.ComposeLoad)
	if err != nil {
		return nil, nil, fmt.Errorf("load compose project for scheduled service %s/%s: %w", ref.Project, ref.Service, err)
	}

	return project, deployConfig, nil
}

// composeConfigFilesExist reports whether all configFiles exist on disk, resolving
// relative paths against workingDir like LoadCompose does.
func composeConfigFilesExist(configFiles []string, workingDir string) bool {
	if len(configFiles) == 0 {
		return false
	}

	for _, f := range configFiles {
		if !filepath.IsAbs(f) {
			f = filepath.Join(workingDir, f)
		}

		if _, err := os.Stat(f); err != nil {
			return false
		}
	}

	return true
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
		SourceType:     strings.TrimSpace(labels[DocoCDLabels.Source.Type]),
		DeploymentName: strings.TrimSpace(labels[DocoCDLabels.Deployment.Name]),
		ConfigTarget:   strings.TrimSpace(labels[DocoCDLabels.Deployment.ConfigTarget]),
		Reference:      strings.TrimSpace(labels[DocoCDLabels.Deployment.TargetRef]),
	}

	return ref, nil
}

// composeScheduledServiceRefFromSwarmLabels builds a composeScheduledServiceRef from a Swarm
// service's labels for cert rotation. Unlike composeScheduledServiceRefFromLabels, Swarm services
// never carry the Compose-managed com.docker.compose.* labels (docker stack deploy doesn't set them),
// so the stack name and working directory are read from doco-cd's own labels instead, and
// ConfigFiles is left empty for loadComposeScheduledProjectAll to fall back to the reloaded
// deploy config. Service is also left empty: RotateProjectCertificates operates on the whole
// stack in Swarm mode rather than a single named service.
func composeScheduledServiceRefFromSwarmLabels(labels map[string]string) (composeScheduledServiceRef, error) {
	if labels == nil {
		return composeScheduledServiceRef{}, fmt.Errorf("%w: missing labels", ErrComposeScheduledMetadataUnavailable)
	}

	project := strings.TrimSpace(labels[DocoCDLabels.Deployment.Name])
	if project == "" {
		return composeScheduledServiceRef{}, fmt.Errorf("%w: missing %q label",
			ErrComposeScheduledMetadataUnavailable,
			DocoCDLabels.Deployment.Name,
		)
	}

	repositoryURL := strings.TrimSpace(labels[DocoCDLabels.Source.URL])
	if repositoryURL == "" {
		repositoryURL = strings.TrimSpace(labels[DocoCDLabels.Source.Name])
	}

	return composeScheduledServiceRef{
		Project:        project,
		WorkingDir:     strings.TrimSpace(labels[DocoCDLabels.Deployment.WorkingDir]),
		RepositoryURL:  repositoryURL,
		SourceType:     strings.TrimSpace(labels[DocoCDLabels.Source.Type]),
		DeploymentName: project,
		ConfigTarget:   strings.TrimSpace(labels[DocoCDLabels.Deployment.ConfigTarget]),
		Reference:      strings.TrimSpace(labels[DocoCDLabels.Deployment.TargetRef]),
	}, nil
}

// loadComposeScheduledDeployConfig reloads the deploy config that originally
// defined the scheduled Compose service and returns the repository root that
// should be used for LoadCompose interpolation and relative path resolution.
func loadComposeScheduledDeployConfig(
	ctx context.Context,
	ref composeScheduledServiceRef,
	secretProvider secretprovider.SecretProvider,
	opts ScheduledComposeOptions,
) (*deploy.Config, string, error) {
	if strings.TrimSpace(ref.RepositoryURL) == "" || strings.TrimSpace(ref.DeploymentName) == "" {
		return nil, "", fmt.Errorf("%w: missing deployment repository and/or name label",
			ErrComposeScheduledMetadataUnavailable)
	}

	dataMountPath := opts.ComposeLoad.DataMountPath

	sourceRepoPath, _, err := resolveScheduledSourceRepo(ref, dataMountPath)
	if err != nil {
		return nil, "", fmt.Errorf("resolve source repository path for scheduled service %s/%s: %w", ref.Project, ref.Service, err)
	}

	// deploy.GetConfigs would fail with a bare ENOENT here. Report the missing source as its own
	// error instead so a deployment whose source has not been fetched yet is distinguishable from
	// one whose config is genuinely broken.
	if !filesystem.IsDir(sourceRepoPath) {
		return nil, "", fmt.Errorf("%w: %s for scheduled service %s/%s",
			ErrComposeScheduledSourceUnavailable, sourceRepoPath, ref.Project, ref.Service)
	}

	repoPath, err := resolveScheduledComposeRepoRoot(ref.WorkingDir, dataMountPath, sourceRepoPath)
	if err != nil {
		return nil, "", err
	}

	configs, err := deploy.GetConfigs(sourceRepoPath, opts.DeployConfigBaseDir, ref.ConfigTarget, ref.Reference, nil)
	if err != nil {
		return nil, "", fmt.Errorf("load deploy config for scheduled service %s/%s: %w", ref.Project, ref.Service, err)
	}

	deployConfig, err := findComposeScheduledDeployConfig(configs, ref.DeploymentName)
	if err != nil {
		return nil, "", err
	}

	// deploy.GetConfigs never sets Internal.ConfigTarget on the returned config (that's normally
	// done by the webhook/poll handler after loading configs for a fresh deploy). Propagate it
	// here from the original deployment's label so any relabeling performed after this reload
	// (e.g. cert rotation redeploys) doesn't blank out the config target label on the recreated
	// service(s), which would otherwise break subsequent reloads that depend on it to pick the
	// correct deployment config file.
	deployConfig.Internal.ConfigTarget = ref.ConfigTarget

	if err = prepareComposeScheduledDeployConfig(ctx, deployConfig, sourceRepoPath, repoPath, secretProvider, opts); err != nil {
		return nil, "", err
	}

	return deployConfig, repoPath, nil
}

// resolveScheduledSourceRepo returns the directory under dataMountPath that holds the
// checked-out Git repository or the extracted OCI artifact for ref, mirroring how
// source.Prepare names it at deploy time: git.GetRepoName for Git sources,
// oci.RepositoryNameFromArtifact for OCI artifacts.
//
// The DocoCDLabels.Source.Type label picks the scheme, but it cannot be trusted on its own:
// deployments created before that label existed, or relabeled by a redeploy path that never knew
// the source type, carry "git" even though their source URL is an OCI artifact reference. The two
// schemes only differ in the ":<tag>" suffix, which git.GetRepoName keeps and
// VerifyAndSanitizePath then rewrites to "_<tag>", so a mislabeled OCI deployment always resolves
// to a directory that was never created on disk. When the labeled scheme's directory is absent but
// the other one exists, use that instead so such deployments still reload. The source type that
// actually resolved is returned alongside the path so callers that relabel the redeployed
// services (see certRotationPayload) write the corrected value back instead of persisting the
// stale one.
func resolveScheduledSourceRepo(ref composeScheduledServiceRef, dataMountPath string) (string, config.SourceType, error) {
	labeled := config.NormalizeSourceType(config.SourceType(ref.SourceType))

	other := config.SourceTypeGit
	if labeled == config.SourceTypeGit {
		other = config.SourceTypeOCI
	}

	preferred := scheduledSourceRepoName(ref.RepositoryURL, labeled)
	alternative := scheduledSourceRepoName(ref.RepositoryURL, other)

	preferredPath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPath, preferred), dataMountPath)
	if err != nil {
		return "", labeled, err
	}

	if preferred == alternative || filesystem.IsDir(preferredPath) {
		return preferredPath, labeled, nil
	}

	alternativePath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPath, alternative), dataMountPath)
	if err == nil && filesystem.IsDir(alternativePath) {
		return alternativePath, other, nil
	}

	return preferredPath, labeled, nil
}

// scheduledSourceRepoName names the data-mount-relative directory that sourceType's fetcher
// extracts repositoryURL into, mirroring source.Prepare.
func scheduledSourceRepoName(repositoryURL string, sourceType config.SourceType) string {
	if sourceType == config.SourceTypeOCI {
		return oci.RepositoryNameFromArtifact(repositoryURL)
	}

	return git.GetRepoName(repositoryURL)
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
	secretProvider secretprovider.SecretProvider,
	opts ScheduledComposeOptions,
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

	if secretProvider == nil || len(deployConfig.ExternalSecrets) == 0 {
		return nil
	}

	interpolatedRefs, err := secrettypes.InterpolateExternalSecretRefs(deployConfig.ExternalSecrets, opts.InterpolateExternalSecrets)
	if err != nil {
		return fmt.Errorf("interpolate external secrets for scheduled service %s: %w", deployConfig.Name, err)
	}

	deployConfig.ExternalSecrets = interpolatedRefs

	encodedSecrets, err := secrettypes.EncodeExternalSecretRefs(interpolatedRefs)
	if err != nil {
		return fmt.Errorf("encode external secrets for scheduled service %s: %w", deployConfig.Name, err)
	}

	resolvedSecrets, err := secretProvider.ResolveSecretReferences(ctx, encodedSecrets)
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
