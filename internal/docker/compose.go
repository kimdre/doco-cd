package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/utils/module"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/utils/set"
	"github.com/kimdre/doco-cd/internal/utils/slice"

	"github.com/go-git/go-git/v5/plumbing/format/diff"

	"github.com/moby/moby/client"

	gitInternal "github.com/kimdre/doco-cd/internal/git"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/webhook"
)

const (
	SocketPath = "/var/run/docker.sock"
)

var (
	ErrNoContainerToStart = errors.New("no container to start")
	ErrIsInUse            = errors.New("is in use")
	ComposeVersion        string // Version of the docker compose module, will be set at runtime
)

func init() {
	version, err := module.GetVersion("github.com/docker/compose/v5")
	if err != nil {
		if errors.Is(err, module.ErrNotFound) {
			ComposeVersion = "unknown"
		} else {
			panic(fmt.Sprintf("failed to get module version: %v", err))
		}
	}

	ComposeVersion = version
}

func CreateDockerCli(quiet, verifyTLS bool) (command.Cli, error) {
	var (
		outputStream io.Writer
		errorStream  io.Writer
	)

	if quiet {
		outputStream = io.Discard
		errorStream = io.Discard
	} else {
		outputStream = os.Stdout
		errorStream = os.Stderr
	}

	dockerCli, err := command.NewDockerCli(
		command.WithOutputStream(outputStream),
		command.WithErrorStream(errorStream),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker cli: %w", err)
	}

	opts := &flags.ClientOptions{Context: "default", LogLevel: "error", TLSVerify: verifyTLS}

	err = dockerCli.Initialize(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize docker cli: %w", err)
	}

	return dockerCli, nil
}

/*
addComposeServiceLabels adds the labels docker compose expects to exist on services.
This is required for future compose operations to work, such as finding
containers that are part of a service.
*/
func addComposeServiceLabels(project *types.Project, deployConfig *config.DeployConfig, payload *webhook.ParsedPayload,
	workingDir, appVersion, timestamp, composeVersion, latestCommit, projectHash string,
) {
	for i, s := range project.Services {
		// Extract service dependencies (depends_on)
		dependencies := make([]string, 0, len(s.DependsOn))
		for dep := range s.DependsOn {
			// https://docs.docker.com/compose/how-tos/startup-order/#control-startup
			// Example: <service>:<condition>:<restart>
			dependencies = append(dependencies, dep)
		}

		s.CustomLabels = map[string]string{
			DocoCDLabels.Metadata.Manager:              config.AppName,
			DocoCDLabels.Metadata.Version:              appVersion,
			DocoCDLabels.Deployment.Name:               deployConfig.Name,
			DocoCDLabels.Deployment.Timestamp:          timestamp,
			DocoCDLabels.Deployment.ComposeHash:        projectHash,
			DocoCDLabels.Deployment.WorkingDir:         workingDir,
			DocoCDLabels.Deployment.Trigger:            payload.CommitSHA,
			DocoCDLabels.Deployment.CommitSHA:          latestCommit,
			DocoCDLabels.Deployment.TargetRef:          deployConfig.Reference,
			DocoCDLabels.Deployment.ConfigHash:         deployConfig.Internal.Hash,
			DocoCDLabels.Deployment.AutoDiscover:       strconv.FormatBool(deployConfig.AutoDiscover),
			DocoCDLabels.Deployment.AutoDiscoverDelete: strconv.FormatBool(deployConfig.AutoDiscoverOpts.Delete),
			DocoCDLabels.Repository.Name:               payload.FullName,
			DocoCDLabels.Repository.URL:                payload.WebURL,
			api.ProjectLabel:                           project.Name,
			api.ServiceLabel:                           s.Name,
			api.WorkingDirLabel:                        project.WorkingDir,
			api.ConfigFilesLabel:                       strings.Join(project.ComposeFiles, ","),
			api.VersionLabel:                           composeVersion,
			api.OneoffLabel:                            "False", // default, will be overridden by docker compose
			api.DependenciesLabel:                      strings.Join(dependencies, ","),
		}
		project.Services[i] = s
	}
}

func addComposeVolumeLabels(project *types.Project, deployConfig *config.DeployConfig, payload *webhook.ParsedPayload,
	appVersion, timestamp, composeVersion, latestCommit, projectHash string,
) {
	for i, v := range project.Volumes {
		v.CustomLabels = map[string]string{
			DocoCDLabels.Metadata.Manager:       config.AppName,
			DocoCDLabels.Metadata.Version:       appVersion,
			DocoCDLabels.Deployment.Name:        deployConfig.Name,
			DocoCDLabels.Deployment.Timestamp:   timestamp,
			DocoCDLabels.Deployment.ComposeHash: projectHash,
			DocoCDLabels.Deployment.Trigger:     payload.CommitSHA,
			DocoCDLabels.Deployment.TargetRef:   deployConfig.Reference,
			DocoCDLabels.Deployment.CommitSHA:   latestCommit,
			DocoCDLabels.Repository.Name:        payload.FullName,
			DocoCDLabels.Repository.URL:         payload.WebURL,
			api.ProjectLabel:                    project.Name,
			api.VolumeLabel:                     v.Name,
			api.VersionLabel:                    composeVersion,
		}
		project.Volumes[i] = v
	}
}

// LoadCompose parses and loads Compose files as specified by the Docker Compose specification.
func LoadCompose(ctx context.Context, repoPath, workingDir, projectName string, composeFiles,
	envFiles, profiles []string, environment map[string]string,
) (*types.Project, error) {
	// Resolve compose file paths to absolute paths relative to workingDir.
	// This is necessary because the compose-go library's LoadConfigFiles internally
	// uses filepath.Abs which resolves relative paths against os.Getwd(), not against
	// the specified working directory. Without this, concurrent deployments with
	// different working directories would fail since they share the same process
	// working directory.
	c, err := config.GetAppConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get app config: %w", err)
	}

	var absComposeFiles []string

	// If the user changed the default compose files, we throw an error of the custom compose file is not found
	throwError := !reflect.DeepEqual(composeFiles, cli.DefaultFileNames)

	for _, f := range composeFiles {
		if !filepath.IsAbs(f) {
			f = filepath.Join(workingDir, f)
		}

		// Check if file exists
		if _, err = os.Stat(f); err != nil {
			if throwError {
				return nil, fmt.Errorf("could not find compose file: %w", err)
			}

			continue
		}

		absComposeFiles = append(absComposeFiles, f)
	}

	// if envFiles only contains ".env", we check if the file exists in the working directory
	if len(envFiles) == 1 && envFiles[0] == ".env" {
		if _, err := os.Stat(path.Join(workingDir, ".env")); errors.Is(err, os.ErrNotExist) {
			envFiles = []string{}
		}
	}

	absEnvFiles := make([]string, 0, len(envFiles))
	for _, f := range envFiles {
		if filepath.IsAbs(f) {
			absEnvFiles = append(absEnvFiles, f)
		} else {
			absEnvFiles = append(absEnvFiles, filepath.Join(workingDir, f))
		}
	}

	var decryptedFiles []string

	decryptFiles := slices.Concat(absComposeFiles, absEnvFiles)
	for _, file := range decryptFiles {
		decrypted, err := encryption.DecryptFileInPlace(file)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt file %s: %w", file, err)
		}

		if decrypted {
			decryptedFiles = append(decryptedFiles, file)
		}
	}

	options, err := cli.NewProjectOptions(
		absComposeFiles,
		cli.WithName(projectName),
		cli.WithWorkingDirectory(workingDir),
		cli.WithInterpolation(true),
		cli.WithResolvedPaths(true),
		cli.WithEnvFiles(absEnvFiles...), // env files for variable interpolation
		cli.WithProfiles(profiles),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create project options: %w", err)
	}

	if len(composeFiles) == 0 {
		err = cli.WithDefaultConfigPath(options)
		if err != nil {
			return nil, fmt.Errorf("failed to use default compose file: %w", err)
		}
	}

	if c.PassEnv {
		err = cli.WithOsEnv(options)
		if err != nil {
			return nil, fmt.Errorf("failed to get OS environment variables for interpolation: %w", err)
		}
	}

	// Inject external secrets into the environment for variable interpolation
	for k, v := range environment {
		options.Environment[k] = v
	}

	err = cli.WithDotEnv(options)
	if err != nil {
		return nil, fmt.Errorf("failed to get .env file for interpolation: %w", err)
	}

	// Preload project for decrypting project-related files
	project, err := options.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load compose project: %w", err)
	}

	// Decrypt any project-related files
	files, err := DecryptProjectFiles(repoPath, project)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt project files: %w", err)
	}

	decryptedFiles = append(decryptedFiles, files...)
	if len(decryptedFiles) > 0 {
		slog.Debug("decrypted SOPS-encrypted files", slog.String("stack", project.Name), slog.Any("files", decryptedFiles))
	}

	// Reload project after decryption to ensure all decrypted values are properly loaded into the project.
	project, err = options.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load compose project: %w", err)
	}

	project, err = project.WithServicesEnvironmentResolved(false)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve services environment: %w", err)
	}

	return project, nil
}

// deployCompose deploys a project as specified by the Docker Compose specification (LoadCompose).
func deployCompose(ctx context.Context, dockerCli command.Cli, project *types.Project,
	deployConfig *config.DeployConfig, recreateMode string, services []string,
	needSignal []SignalService,
) error {
	var (
		err          error
		beforeImages map[string]api.ImageSummary // Images used by stack before deployment
		afterImages  map[string]api.ImageSummary // Images used by stack after deployment
	)

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	if len(needSignal) > 0 {
		if err := ComposeSignal(ctx, dockerCli, project, needSignal); err != nil {
			return err
		}
	}

	if deployConfig.PruneImages {
		beforeImages, err = service.Images(ctx, project.Name, api.ImagesOptions{})
		if err != nil {
			return fmt.Errorf("failed to get existing images: %w", err)
		}
	}

	if deployConfig.ForceImagePull {
		for i, s := range project.Services {
			s.PullPolicy = types.PullPolicyAlways
			project.Services[i] = s
		}

		err = service.Pull(ctx, project, api.PullOptions{
			Quiet: true,
		})
		if err != nil {
			return fmt.Errorf("failed to pull images: %w", err)
		}
	}

	if recreateMode == "" {
		recreateMode = api.RecreateDiverged
	}

	// Convert deployConfig.BuildOpts.Args to types.MappingWithEquals
	buildArgs := make(types.MappingWithEquals)
	for k, v := range deployConfig.BuildOpts.Args {
		buildArgs[k] = &v
	}

	buildOpts := api.BuildOptions{
		Pull:     deployConfig.BuildOpts.ForceImagePull,
		Quiet:    deployConfig.BuildOpts.Quiet,
		Progress: "auto",
		Args:     buildArgs,
		NoCache:  deployConfig.BuildOpts.NoCache,
	}

	err = service.Build(ctx, project, buildOpts)
	if err != nil {
		return err
	}

	createOpts := api.CreateOptions{
		Services:             services,
		RemoveOrphans:        deployConfig.RemoveOrphans,
		Recreate:             recreateMode,
		RecreateDependencies: api.RecreateDiverged,
		QuietPull:            true,
	}

	startOpts := api.StartOptions{
		Project:     project,
		Wait:        true,
		WaitTimeout: time.Duration(deployConfig.Timeout) * time.Second,
	}

	err = service.Up(ctx, project, api.UpOptions{
		Create: createOpts,
		Start:  startOpts,
	})
	if err != nil {
		if errors.Is(err, ErrNoContainerToStart) {
			err = service.Start(ctx, project.Name, startOpts)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	if deployConfig.PruneImages {
		afterImages, err = service.Images(ctx, project.Name, api.ImagesOptions{})
		if err != nil {
			return fmt.Errorf("failed to get images after deployment: %w", err)
		}

		// Determine unused images by comparing image SHAs used by services before and after the deployment

		var ids []string

		for svc, beforeImg := range beforeImages {
			afterImg, exists := afterImages[svc]
			if !exists || beforeImg.ID != afterImg.ID {
				ids = append(ids, beforeImg.ID)
			}
		}

		_, err = pruneImages(ctx, dockerCli, slice.Unique(ids))
		if err != nil {
			return fmt.Errorf("failed to prune images: %w", err)
		}
	}

	return nil
}

// DeployStack deploys the stack using the provided deployment configuration.
func DeployStack(
	jobLog *slog.Logger, externalRepoPath string, ctx *context.Context,
	dockerCli *command.Cli, dockerClient *client.Client, payload *webhook.ParsedPayload, deployConfig *config.DeployConfig,
	detectedChanges []Change, needSignal []SignalService, latestCommit, appVersion string, forceDeploy bool,
) error {
	startTime := time.Now()

	stackLog := jobLog.
		With(slog.String("stack", deployConfig.Name))

	// Path on the host
	externalWorkingDir := path.Join(externalRepoPath, deployConfig.WorkingDirectory)

	externalWorkingDir, err := filepath.Abs(externalWorkingDir)
	if err != nil || !strings.HasPrefix(externalWorkingDir, externalRepoPath) {
		errMsg := "invalid working directory: resolved path is outside the allowed base directory"
		jobLog.Error(errMsg, slog.String("resolved_path", externalWorkingDir))

		return fmt.Errorf("%s", errMsg)
	}

	project, err := LoadCompose(*ctx, externalRepoPath, externalWorkingDir, deployConfig.Name, deployConfig.ComposeFiles,
		deployConfig.EnvFiles, deployConfig.Profiles, deployConfig.Internal.Environment)
	if err != nil {
		return fmt.Errorf("failed to load compose config: %w", err)
	}

	done := make(chan struct{})
	defer close(done)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				stackLog.Info("deployment in progress")
			case <-done:
				return
			}
		}
	}()

	timestamp := time.Now().UTC().Format(time.RFC3339)

	// Generate project hash with doco-cd labels
	// We don't want to compare the hashes with these labels
	projectHash, err := ProjectHash(project)
	if err != nil {
		return fmt.Errorf("failed to generate project hash: %w", err)
	}

	// When SwarmModeEnabled is true, we deploy the stack using Docker Swarm.
	if swarm.ModeEnabled {
		stackLog.Info("deploying swarm stack")

		cfg, opts, err := LoadSwarmStack(dockerCli, project, deployConfig, externalWorkingDir)
		if err != nil {
			return fmt.Errorf("failed to load swarm stack: %w", err)
		}

		addSwarmServiceLabels(cfg, deployConfig, payload, externalWorkingDir, appVersion, timestamp, latestCommit, projectHash)
		addSwarmVolumeLabels(cfg, deployConfig, payload, externalWorkingDir, appVersion, timestamp, latestCommit)
		addSwarmConfigLabels(cfg, deployConfig, payload, externalWorkingDir, appVersion, timestamp, latestCommit)
		addSwarmSecretLabels(cfg, deployConfig, payload, externalWorkingDir, appVersion, timestamp, latestCommit)

		err = DeploySwarmStack(*ctx, *dockerCli, cfg, opts)
		if err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(deployConfig.Name).Inc()

			errMsg := "failed to deploy swarm stack " + deployConfig.Name

			return fmt.Errorf("%s: %w", errMsg, err)
		}

		err = PruneStackConfigs(*ctx, dockerClient, deployConfig.Name)
		if err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(deployConfig.Name).Inc()

			errMsg := "failed to prune stack configs"

			return fmt.Errorf("%s: %w", errMsg, err)
		}

		err = PruneStackSecrets(*ctx, dockerClient, deployConfig.Name)
		if err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(deployConfig.Name).Inc()

			errMsg := "failed to prune stack secrets"

			return fmt.Errorf("%s: %w", errMsg, err)
		}

		if deployConfig.PruneImages {
			stackLog.Info("prune images on swarm nodes")

			err = RunImagePruneJob(*ctx, *dockerCli)
			if err != nil {
				prometheus.DeploymentErrorsTotal.WithLabelValues(deployConfig.Name).Inc()

				errMsg := "failed to run image prune job"

				return fmt.Errorf("%s: %w", errMsg, err)
			}
		}
	} else {
		addComposeServiceLabels(project, deployConfig, payload, externalWorkingDir, appVersion, timestamp, ComposeVersion, latestCommit, projectHash)
		addComposeVolumeLabels(project, deployConfig, payload, appVersion, timestamp, ComposeVersion, latestCommit, projectHash)

		forcedServices := set.New[string]() // services to recreate if project files changed
		recreateMode := api.RecreateDiverged

		switch {
		case forceDeploy:
			recreateMode = api.RecreateForce

			stackLog.Debug("force deploy enabled, forcing recreate of all services")
		case len(detectedChanges) > 0:
			recreateMode = api.RecreateForce

			for _, change := range detectedChanges {
				forcedServices.Add(change.Services...)
			}

			stackLog.Debug("changed project files detected, forcing recreate", slog.Any("changes", detectedChanges))
		case len(needSignal) > 0:
			stackLog.Debug("changed project files detected, sending signal to service",
				slog.Any("need_signal", needSignal))
		}

		stackLog.Info("deploying stack",
			slog.Group("recreate",
				slog.String("mode", recreateMode),
				slog.Any("forced_services", forcedServices.ToSlice()),
			),
			slog.Any("need_signal", needSignal),
		)

		err = deployCompose(*ctx, *dockerCli, project, deployConfig, recreateMode,
			forcedServices.ToSlice(), needSignal)
		if err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(deployConfig.Name).Inc()
			return fmt.Errorf("failed to deploy stack: %w", err)
		}
	}

	prometheus.DeploymentsTotal.WithLabelValues(deployConfig.Name).Inc()
	prometheus.DeploymentDuration.WithLabelValues(deployConfig.Name).Observe(time.Since(startTime).Seconds())

	return nil
}

// DestroyStack destroys the stack using the provided deployment configuration.
func DestroyStack(
	jobLog *slog.Logger, ctx *context.Context,
	dockerCli *command.Cli, deployConfig *config.DeployConfig,
) error {
	stackLog := jobLog.
		With(slog.String("stack", deployConfig.Name))

	stackLog.Info("destroying stack")

	if swarm.ModeEnabled {
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
		Volumes:       deployConfig.DestroyOpts.RemoveVolumes,
	}

	if deployConfig.DestroyOpts.RemoveImages {
		downOpts.Images = "all"
	}

	err = service.Down(*ctx, deployConfig.Name, downOpts)
	if err != nil {
		errMsg := "failed to destroy stack"
		return fmt.Errorf("%s: %w", errMsg, err)
	}

	return nil
}

func GetPathsFromGitChangedFiles(changedFiles []gitInternal.ChangedFile, basePath string) []string {
	var absPaths []string

	basePath = filepath.Clean(basePath)

	for _, f := range changedFiles {
		checkPaths := []diff.File{f.From, f.To}

		for _, checkPath := range checkPaths {
			if checkPath == nil {
				continue
			}

			p := filepath.Clean(checkPath.Path())

			if !filepath.IsAbs(p) {
				p = filepath.Join(basePath, p)
			}

			absPaths = append(absPaths, p)
		}
	}

	return slice.Unique(absPaths)
}

// HasChangedConfigs checks if any files used in docker compose `configs:` definitions have changed using the Git status.
func HasChangedConfigs(repoPathExternal string, paths []string, project *types.Project, ignoreCfg projectIgnoreCfg) ([]string, []string) {
	configToServicesMap := make(map[string][]string)

	for name, s := range project.Services {
		for _, cfg := range s.Configs {
			configToServicesMap[cfg.Source] = append(configToServicesMap[cfg.Source], name)
		}
	}

	var (
		changedServices []string
		ignoredServices []string
	)

	for cfgName, c := range project.Configs {
		// Changes in config.Content are handled in project hash comparison
		if c.File == "" {
			continue
		}

		for _, p := range paths {
			// ignore change outside repo
			if filesystem.InBasePath(c.File, p) &&
				filesystem.InBasePath(repoPathExternal, c.File) {
				for _, svcName := range configToServicesMap[cfgName] {
					if !checkIsIgnoreByCfg(ignoreCfg, svcName, changeScopeConfigs, cfgName) {
						changedServices = append(changedServices, svcName)
					} else {
						ignoredServices = append(ignoredServices, svcName)
					}
				}
			}
		}
	}

	return getChangeAndIgnore(changedServices, ignoredServices)
}

// HasChangedSecrets checks if any files used in docker compose `secrets:` definitions have changed using the Git status.
func HasChangedSecrets(repoPathExternal string, paths []string, project *types.Project, ignoreCfg projectIgnoreCfg) ([]string, []string) {
	secretsToServicesMap := make(map[string][]string)

	for name, s := range project.Services {
		for _, secret := range s.Secrets {
			secretsToServicesMap[secret.Source] = append(secretsToServicesMap[secret.Source], name)
		}
	}

	var (
		changedServices []string
		ignoredServices []string
	)

	for secretName, s := range project.Secrets {
		if s.File == "" {
			continue
		}

		for _, p := range paths {
			// ignore change outside repo
			if filesystem.InBasePath(s.File, p) &&
				filesystem.InBasePath(repoPathExternal, s.File) {
				for _, svcName := range secretsToServicesMap[secretName] {
					if !checkIsIgnoreByCfg(ignoreCfg, svcName, changeScopeSecrets, secretName) {
						changedServices = append(changedServices, svcName)
					} else {
						ignoredServices = append(ignoredServices, svcName)
					}
				}
			}
		}
	}

	return getChangeAndIgnore(changedServices, ignoredServices)
}

// HasChangedBindMounts checks if any files used in docker compose `volumes:` definitions with type `bind` have changed using the Git status.
func HasChangedBindMounts(repoPathExternal string, paths []string, project *types.Project, ignoreCfg projectIgnoreCfg) ([]string, []string) {
	var (
		changedServices []string
		ignoredServices []string
	)

	for _, s := range project.Services {
	out:
		for _, v := range s.Volumes {
			if v.Type == "bind" && v.Source != "" {
				for _, p := range paths {
					// ignore change outside repo
					if filesystem.InBasePath(v.Source, p) &&
						filesystem.InBasePath(repoPathExternal, v.Source) {
						if !checkIsIgnoreByCfg(ignoreCfg, s.Name, changeScopeBindMounts, v.Target) {
							changedServices = append(changedServices, s.Name)
						} else {
							ignoredServices = append(ignoredServices, s.Name)
						}

						break out
					}
				}
			}
		}
	}

	return getChangeAndIgnore(changedServices, ignoredServices)
}

// HasChangedEnvFiles checks if any files used in docker compose `env_file:` definitions have changed using the Git status.
func HasChangedEnvFiles(repoPathExternal string, paths []string, project *types.Project, _ projectIgnoreCfg) ([]string, []string) {
	var changedServices []string

	for _, s := range project.Services {
	out:
		for _, envFile := range s.EnvFiles {
			for _, p := range paths {
				// ignore change outside repo
				if filesystem.InBasePath(envFile.Path, p) &&
					filesystem.InBasePath(repoPathExternal, envFile.Path) {
					changedServices = append(changedServices, s.Name)
					break out
				}
			}
		}
	}

	return slice.Unique(changedServices), nil
}

// HasChangedBuildFiles checks if any files used as build context in docker compose `build:` definitions have changed using the Git status.
// This includes any file within the build context directory for each service. If a changed file is within a build context, it returns true.
func HasChangedBuildFiles(repoPathExternal string, paths []string, project *types.Project, _ projectIgnoreCfg) ([]string, []string) {
	var changedServices []string

	for _, s := range project.Services {
		if s.Build == nil {
			continue
		}

		buildContext := s.Build.Context
		additionalContexts := s.Build.AdditionalContexts
		dockerFile := s.Build.Dockerfile
		buildSecrets := s.Build.Secrets

		if buildContext == "" && len(additionalContexts) == 0 && dockerFile == "" && len(buildSecrets) == 0 {
			continue
		}

		var contexts []string

		if buildContext != "" {
			contexts = append(contexts, buildContext)
		}

		for _, v := range additionalContexts {
			if v != "" {
				contexts = append(contexts, v)
			}
		}

		for _, secret := range buildSecrets {
			if secret.Source != "" {
				contexts = append(contexts, secret.Source)
			}
		}

		if dockerFile != "" {
			contexts = append(contexts, dockerFile)
		}

	out:

		for _, ctxFile := range contexts {
			if !path.IsAbs(ctxFile) {
				ctxFile = filepath.Join(project.WorkingDir, ctxFile)
			}

			for _, p := range paths {
				// ignore change outside repo
				if filesystem.InBasePath(ctxFile, p) &&
					filesystem.InBasePath(repoPathExternal, ctxFile) {
					changedServices = append(changedServices, s.Name)
					break out
				}
			}
		}
	}

	return slice.Unique(changedServices), nil
}

type Change struct {
	Type     string
	Services []string
}

// sortChanges sorts the changes first by type and then by service name within each change.
func sortChanges(changes []Change) {
	slices.SortFunc(changes, func(a, b Change) int {
		return strings.Compare(a.Type, b.Type)
	})

	for i := range changes {
		slices.Sort(changes[i].Services)
	}
}

type IgnoredInfo struct {
	// Ignored services name
	Ignored []string `json:"ignored"`
	// Ignored services need to send signal
	NeedSendSignal []SignalService `json:"need_signal"`
}

func (i IgnoredInfo) IsEmpty() bool {
	return len(i.Ignored) == 0 && len(i.NeedSendSignal) == 0
}

type SignalService struct {
	ServiceName string `json:"service_name"`
	Signal      string `json:"signal"`
}

// ProjectFilesHaveChanges checks if any files related to the compose project have changed.
func ProjectFilesHaveChanges(repoPathExternal string, changePaths []string, project *types.Project) ([]Change, IgnoredInfo, error) {
	checks := []struct {
		name changeScope
		fn   func(string, []string, *types.Project, projectIgnoreCfg) ([]string, []string)
	}{
		{changeScopeConfigs, HasChangedConfigs},
		{changeScopeSecrets, HasChangedSecrets},
		{changeScopeBindMounts, HasChangedBindMounts},
		{changeScopeEnvFiles, HasChangedEnvFiles},
		{changeScopeBuildFiles, HasChangedBuildFiles},
	}

	ignoreCfg, err := getIgnoreRecreateCfgFromProject(project)
	if err != nil {
		return nil, IgnoredInfo{}, err
	}

	var (
		changes                                []Change
		allChangedServices, allIgnoredServices []string
	)

	for _, check := range checks {
		changedServices, ignoredServices := check.fn(repoPathExternal, changePaths, project, ignoreCfg)

		allChangedServices = append(allChangedServices, changedServices...)
		allIgnoredServices = append(allIgnoredServices, ignoredServices...)

		if len(changedServices) > 0 {
			slices.Sort(changedServices)

			changes = append(changes, Change{
				Type:     string(check.name),
				Services: changedServices,
			})
		}
	}

	sortChanges(changes)

	_, ignores := getChangeAndIgnore(allChangedServices, allIgnoredServices)
	slices.Sort(ignores)

	retIgnored := IgnoredInfo{}

	for _, svcName := range ignores {
		sig := ignoreCfg[svcName].signal
		if sig != "" {
			retIgnored.NeedSendSignal = append(retIgnored.NeedSendSignal, SignalService{
				ServiceName: svcName,
				Signal:      sig,
			})
		} else {
			retIgnored.Ignored = append(retIgnored.Ignored, svcName)
		}
	}

	return changes, retIgnored, nil
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

	return service.Down(ctx, projectName, api.DownOptions{
		RemoveOrphans: true,
		Timeout:       &timeout,
		Volumes:       removeVolumes,
		Images: func() string {
			if removeImages {
				return "all"
			}

			return "local"
		}(),
	})
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

// pruneImages tries to remove the specified image IDs from the Docker host and returns a list of pruned image IDs.
// If an image is still in use by a running container, the image won't be removed.
func pruneImages(ctx context.Context, dockerCli command.Cli, images []string) ([]string, error) {
	var prunedImages []string

	for _, img := range images {
		result, err := dockerCli.Client().ImageRemove(ctx, img, client.ImageRemoveOptions{
			Force:         true,
			PruneChildren: true,
		})
		if err != nil {
			if strings.Contains(err.Error(), "image is being used by running container") {
				// Ignore error if image is being used by a running container
				continue
			}

			if strings.Contains(err.Error(), "no such image") || strings.Contains(err.Error(), "not found") {
				// Ignore error if image does not exist
				continue
			}

			return nil, fmt.Errorf("failed to remove image %s: %w", img, err)
		}

		for _, r := range result.Items {
			if r.Deleted != "" {
				prunedImages = append(prunedImages, r.Deleted)
			} else if r.Untagged != "" {
				prunedImages = append(prunedImages, r.Untagged)
			}
		}
	}

	return prunedImages, nil
}

// PullImages pulls all images defined in the compose project.
func PullImages(ctx context.Context, dockerCli command.Cli, projectName string) error {
	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	containers, err := GetProjectContainers(ctx, dockerCli, projectName)
	if err != nil {
		return fmt.Errorf("failed to get project containers: %w", err)
	}

	containerNames := make([]string, 0, len(containers))
	for _, c := range containers {
		containerNames = append(containerNames, c.Name)
	}

	project, err := service.Generate(ctx, api.GenerateOptions{ProjectName: projectName, Containers: containerNames})
	if err != nil {
		return fmt.Errorf("failed to generate project: %w", err)
	}

	return service.Pull(ctx, project, api.PullOptions{
		Quiet: true,
	})
}

// GetImages retrieves all image IDs used by the services in the compose project.
func GetImages(ctx context.Context, dockerCli command.Cli, projectName string) (set.Set[string], error) {
	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return nil, err
	}

	imageSummaries, err := service.Images(ctx, projectName, api.ImagesOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get images: %w", err)
	}

	images := set.New[string]()
	for _, img := range imageSummaries {
		images.Add(img.ID)
	}

	return images, nil
}

// CheckDefaultComposeFiles checks if the default compose files are used and returns them if true.
func CheckDefaultComposeFiles(composeFiles []string, workingDir string) ([]string, error) {
	if reflect.DeepEqual(composeFiles, cli.DefaultFileNames) {
		var (
			err             error
			tmpComposeFiles []string
		)

		// Check if the default compose files exist

		for _, f := range composeFiles {
			if _, err = os.Stat(path.Join(workingDir, f)); errors.Is(err, os.ErrNotExist) {
				continue
			}

			tmpComposeFiles = append(tmpComposeFiles, f)
		}

		if len(tmpComposeFiles) == 0 {
			errMsg := "no compose files found"
			return nil, fmt.Errorf("%s: %w", errMsg, err)
		}

		return tmpComposeFiles, nil
	}

	return composeFiles, nil
}

// DecryptProjectFiles decrypts all files used in the compose project that are encrypted using doco-cd's encryption mechanism.
// This includes configs, secrets, bind mounts, env files and build contexts.
// Since absolute file paths in types.Project are paths on the docker host, repoPath also needs to be the external path to the repository.
// We use the symlink inside the container to follow the external path to the correct internal path.
func DecryptProjectFiles(repoPath string, p *types.Project) ([]string, error) {
	var (
		projectFiles   []string
		decryptedFiles []string
	)

	for _, s := range p.Services {
		for _, cfg := range s.Configs {
			if cfg.Source != "" {
				if cfgConfig, ok := p.Configs[cfg.Source]; ok && cfgConfig.File != "" {
					projectFiles = append(projectFiles, cfgConfig.File)
				}
			}
		}

		for _, secret := range s.Secrets {
			if secret.Source != "" {
				if secretConfig, ok := p.Secrets[secret.Source]; ok && secretConfig.File != "" {
					projectFiles = append(projectFiles, secretConfig.File)
				}
			}
		}

		for _, v := range s.Volumes {
			if v.Type == "bind" && v.Source != "" {
				info, err := os.Stat(v.Source)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}

					return decryptedFiles, fmt.Errorf("failed to stat bind mount source '%s': %w", v.Source, err)
				}

				if info.IsDir() {
					decryptedFiles, err = encryption.DecryptFilesInDirectory(repoPath, v.Source)
					if err != nil {
						if errors.Is(err, filesystem.ErrPathTraversal) {
							continue
						}

						return decryptedFiles, fmt.Errorf("failed to decrypt files in bind mount directory '%s': %w", v.Source, err)
					}

					continue
				}

				projectFiles = append(projectFiles, v.Source)
			}
		}

		for _, envFile := range s.EnvFiles {
			if envFile.Path != "" {
				projectFiles = append(projectFiles, envFile.Path)
			}
		}

		if s.Build != nil {
			if s.Build.Dockerfile != "" {
				if filepath.IsAbs(s.Build.Dockerfile) {
					projectFiles = append(projectFiles, s.Build.Dockerfile)
				} else {
					projectFiles = append(projectFiles, filepath.Join(s.Build.Context, s.Build.Dockerfile))
				}
			}

			for _, secret := range s.Build.Secrets {
				if secret.Source != "" {
					if filepath.IsAbs(secret.Source) {
						projectFiles = append(projectFiles, secret.Source)
					} else {
						projectFiles = append(projectFiles, filepath.Join(s.Build.Context, secret.Source))
					}
				}
			}
		}
	}

	for _, f := range slice.Unique(projectFiles) {
		if !filepath.IsAbs(f) {
			f = filepath.Join(p.WorkingDir, f)
		}

		decrypted, err := encryption.DecryptFileInPlace(f)
		if err != nil {
			return decryptedFiles, fmt.Errorf("failed to decrypt project file '%s': %w", f, err)
		}

		if decrypted {
			decryptedFiles = append(decryptedFiles, f)
		}
	}

	return decryptedFiles, nil
}
