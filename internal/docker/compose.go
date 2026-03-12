package docker

import (
	"context"
	"encoding/json"
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

	"github.com/opencontainers/go-digest"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
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
	"github.com/kimdre/doco-cd/internal/logger"
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
func addComposeServiceLabels(project *types.Project, deployConfig config.DeployConfig, payload webhook.ParsedPayload, repoDir, appVersion, timestamp, composeVersion, latestCommit string) {
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
			DocoCDLabels.Deployment.ComposeHash:        ProjectHash(project),
			DocoCDLabels.Deployment.WorkingDir:         repoDir,
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

func addComposeVolumeLabels(project *types.Project, deployConfig config.DeployConfig, payload webhook.ParsedPayload, appVersion, timestamp, composeVersion, latestCommit string) {
	for i, v := range project.Volumes {
		v.CustomLabels = map[string]string{
			DocoCDLabels.Metadata.Manager:       config.AppName,
			DocoCDLabels.Metadata.Version:       appVersion,
			DocoCDLabels.Deployment.Name:        deployConfig.Name,
			DocoCDLabels.Deployment.Timestamp:   timestamp,
			DocoCDLabels.Deployment.ComposeHash: ProjectHash(project),
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
func LoadCompose(ctx context.Context, workingDir, projectName string, composeFiles, envFiles, profiles []string, environment map[string]string) (*types.Project, error) {
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

	absoluteComposeFiles := make([]string, len(composeFiles))
	for i, f := range composeFiles {
		if filepath.IsAbs(f) {
			absoluteComposeFiles[i] = f
		} else {
			absoluteComposeFiles[i] = filepath.Join(workingDir, f)
		}
	}

	// if envFiles only contains ".env", we check if the file exists in the working directory
	if len(envFiles) == 1 && envFiles[0] == ".env" {
		if _, err := os.Stat(path.Join(workingDir, ".env")); errors.Is(err, os.ErrNotExist) {
			envFiles = []string{}
		}
	}

	absoluteEnvFiles := make([]string, 0, len(envFiles))
	for _, f := range envFiles {
		if filepath.IsAbs(f) {
			absoluteEnvFiles = append(absoluteEnvFiles, f)
		} else {
			absoluteEnvFiles = append(absoluteEnvFiles, filepath.Join(workingDir, f))
		}
	}

	options, err := cli.NewProjectOptions(
		absoluteComposeFiles,
		cli.WithName(projectName),
		cli.WithWorkingDirectory(workingDir),
		cli.WithInterpolation(true),
		cli.WithResolvedPaths(true),
		cli.WithEnvFiles(absoluteEnvFiles...), // env files for variable interpolation
		cli.WithProfiles(profiles),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create project options: %w", err)
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

	project, err := options.LoadProject(ctx)
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
	deployConfig *config.DeployConfig, payload webhook.ParsedPayload,
	repoDir, latestCommit, appVersion string,
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

	timestamp := time.Now().UTC().Format(time.RFC3339)

	if ComposeVersion == "" {
		ComposeVersion, err = GetModuleVersion("github.com/docker/compose/v5")
		if err != nil {
			if errors.Is(err, ErrModuleNotFound) {
				// Placeholder for when the module is not found
				ComposeVersion = "unknown"
			} else {
				return fmt.Errorf("failed to get module version: %w", err)
			}
		}
	}

	if deployConfig.PruneImages {
		beforeImages, err = service.Images(ctx, project.Name, api.ImagesOptions{})
		if err != nil {
			return fmt.Errorf("failed to get existing images: %w", err)
		}
	}

	addComposeServiceLabels(project, *deployConfig, payload, repoDir, appVersion, timestamp, ComposeVersion, latestCommit)
	addComposeVolumeLabels(project, *deployConfig, payload, appVersion, timestamp, ComposeVersion, latestCommit)

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

	recreateType := api.RecreateDiverged
	if deployConfig.ForceRecreate {
		recreateType = api.RecreateForce
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
		RemoveOrphans:        deployConfig.RemoveOrphans,
		Recreate:             recreateType,
		RecreateDependencies: recreateType,
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
	jobLog *slog.Logger, externalRepoPath, internalRepoPath string, ctx *context.Context,
	dockerCli *command.Cli, dockerClient *client.Client, payload *webhook.ParsedPayload, deployConfig *config.DeployConfig,
	changedFiles []gitInternal.ChangedFile, latestCommit, appVersion, triggerEvent string, forceDeploy bool,
	resolvedSecrets secrettypes.ResolvedSecrets, secretsChanged bool,
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

	internalWorkingDir := path.Join(internalRepoPath, deployConfig.WorkingDirectory)

	internalWorkingDir, err = filepath.Abs(internalWorkingDir)
	if err != nil || !strings.HasPrefix(internalWorkingDir, internalRepoPath) {
		errMsg := "invalid working directory: resolved path is outside the allowed base directory"
		jobLog.Error(errMsg, slog.String("resolved_path", internalWorkingDir))

		return fmt.Errorf("%s", errMsg)
	}

	// Create a temporary env file if environment variables are specified in the deployment config
	if deployConfig.Internal.Environment != nil {
		tmpEnvFile, err := config.CreateTmpDotEnvFile(deployConfig)
		if err != nil {
			errMsg := "failed to create temporary env file"
			return fmt.Errorf("%s: %w", errMsg, err)
		}

		// Delete the temp file after deployment
		defer func(name string) {
			err = os.Remove(name)
			if err != nil {
				stackLog.Warn("failed to delete temporary env file", logger.ErrAttr(err), slog.String("file", name))
			}
		}(tmpEnvFile)
	}

	project, err := LoadCompose(*ctx, externalWorkingDir, deployConfig.Name, deployConfig.ComposeFiles, deployConfig.EnvFiles, deployConfig.Profiles, resolvedSecrets)
	if err != nil {
		errMsg := "failed to load compose config"
		return fmt.Errorf("%s: %w", errMsg, err)
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

	// When SwarmModeEnabled is true, we deploy the stack using Docker Swarm.
	if swarm.ModeEnabled {
		stackLog.Info("deploying swarm stack")

		err = DeploySwarmStack(*ctx, *dockerCli, project, deployConfig, *payload, externalWorkingDir, latestCommit, appVersion, resolvedSecrets)
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
		detectedChanges, err := ProjectFilesHaveChanges(changedFiles, project, internalWorkingDir)
		if err != nil {
			errMsg := "failed to check for changed project files"
			return fmt.Errorf("%s: %w", errMsg, err)
		}

		hasChangedCompose, err := HasChangedComposeFiles(changedFiles, project)
		if err != nil {
			errMsg := "failed to check for changed compose files"
			return fmt.Errorf("%s: %w", errMsg, err)
		}

		switch {
		case forceDeploy:
			deployConfig.ForceRecreate = true

			stackLog.Debug("force deploy enabled, forcing recreate of all services")
		case secretsChanged:
			deployConfig.ForceRecreate = true

			stackLog.Debug("changed external secrets detected, forcing recreate of all services")
		case len(detectedChanges) > 0 || (hasChangedCompose && triggerEvent == "poll"):
			deployConfig.ForceRecreate = true

			stackLog.Debug("changed project files detected, forcing recreate of all services", slog.Any("changed_files", detectedChanges))
		case hasChangedCompose:
			stackLog.Debug("changed compose files detected, continue normal deployment")
		}

		stackLog.Info("deploying stack", slog.Bool("forced", deployConfig.ForceRecreate))

		err = deployCompose(*ctx, *dockerCli, project, deployConfig, *payload, externalWorkingDir, latestCommit, appVersion)
		if err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(deployConfig.Name).Inc()

			errMsg := "failed to deploy stack"

			return fmt.Errorf("%s: %w", errMsg, err)
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

func getAbsolutePaths(changedFiles []gitInternal.ChangedFile, repoRoot string) []string {
	var absPaths []string

	repoRoot = filepath.Clean(repoRoot)

	for _, f := range changedFiles {
		checkPaths := []diff.File{f.From, f.To}

		for _, checkPath := range checkPaths {
			if checkPath == nil {
				continue
			}

			p := filepath.Clean(checkPath.Path())

			if !filepath.IsAbs(p) {
				p = filepath.Join(repoRoot, p)
			}

			if !slices.Contains(absPaths, p) {
				absPaths = append(absPaths, p)
			}
		}
	}

	return absPaths
}

// HasChangedConfigs checks if any files used in docker compose `configs:` definitions have changed using the Git status.
func HasChangedConfigs(changedFiles []gitInternal.ChangedFile, project *types.Project, _ string) (bool, error) {
	// We only need the relative part in this case
	paths := getAbsolutePaths(changedFiles, ".")

	for _, c := range project.Configs {
		// Changes in config.Content are handled in project hash comparison
		if c.File == "" {
			continue
		}

		for _, p := range paths {
			if strings.HasSuffix(c.File, p) {
				return true, nil
			}
		}
	}

	return false, nil
}

// HasChangedSecrets checks if any files used in docker compose `secrets:` definitions have changed using the Git status.
func HasChangedSecrets(changedFiles []gitInternal.ChangedFile, project *types.Project, _ string) (bool, error) {
	// We only need the relative part in this case
	paths := getAbsolutePaths(changedFiles, ".")

	for _, s := range project.Secrets {
		if s.File == "" {
			continue
		}

		for _, p := range paths {
			if strings.HasSuffix(s.File, p) {
				return true, nil
			}
		}
	}

	return false, nil
}

// HasChangedBindMounts checks if any files used in docker compose `volumes:` definitions with type `bind` have changed using the Git status.
func HasChangedBindMounts(changedFiles []gitInternal.ChangedFile, project *types.Project, absWorkingDir string) (bool, error) {
	paths := getAbsolutePaths(changedFiles, ".")

	for _, s := range project.Services {
		for _, v := range s.Volumes {
			if v.Type == "bind" && v.Source != "" {
				bindSourceAbs := v.Source
				if !filepath.IsAbs(bindSourceAbs) {
					bindSourceAbs = filepath.Join(absWorkingDir, bindSourceAbs)
				}

				for _, p := range paths {
					// If first part of p and last part of bindSourceAbs contain the same string, remove if from p and join them together
					if strings.HasSuffix(p, filepath.Base(bindSourceAbs)) {
						bindSourceAbs = strings.TrimSuffix(bindSourceAbs, filepath.Base(bindSourceAbs))
						p = filepath.Join(bindSourceAbs, p)
					}

					rel, err := filepath.Rel(bindSourceAbs, p)
					if err != nil {
						continue
					}

					if !strings.HasPrefix(rel, "..") {
						return true, nil
					}
				}
			}
		}
	}

	return false, nil
}

// HasChangedEnvFiles checks if any files used in docker compose `env_file:` definitions have changed using the Git status.
func HasChangedEnvFiles(changedFiles []gitInternal.ChangedFile, project *types.Project, _ string) (bool, error) {
	// We only need the relative part in this case
	paths := getAbsolutePaths(changedFiles, ".")

	for _, s := range project.Services {
		for _, envFile := range s.EnvFiles {
			for _, p := range paths {
				if strings.HasSuffix(envFile.Path, p) {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// HasChangedBuildFiles checks if any files used as build context in docker compose `build:` definitions have changed using the Git status.
// This includes any file within the build context directory for each service. If a changed file is within a build context, it returns true.
func HasChangedBuildFiles(changedFiles []gitInternal.ChangedFile, project *types.Project, _ string) (bool, error) {
	paths := getAbsolutePaths(changedFiles, ".")

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

		for _, ctxFile := range contexts {
			if !path.IsAbs(ctxFile) {
				ctxFile = filepath.Join(project.WorkingDir, ctxFile)
			}

			for _, p := range paths {
				if strings.HasSuffix(ctxFile, p) {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// HasChangedComposeFiles checks if any of the compose files have changed using the Git status.
func HasChangedComposeFiles(changedFiles []gitInternal.ChangedFile, project *types.Project) (bool, error) {
	// Get absolute paths of changed files
	changedPaths := getAbsolutePaths(changedFiles, project.WorkingDir)

	for _, file := range project.ComposeFiles {
		changed, err := checkFilePath(file, changedPaths, project.WorkingDir)
		if err != nil {
			return false, err
		}

		if changed {
			return true, nil
		}
	}

	return false, nil
}

// checkFilePath checks if the given file path matches any of the paths in the list,
// considering both absolute and relative paths and allowing for matching based on the last 4 parts of the path.
func checkFilePath(file string, paths []string, workingDir string) (bool, error) {
	if !path.IsAbs(file) {
		file = filepath.Join(workingDir, file)
	}

	file = filepath.Clean(file)

	// Get the last 4 parts of the file path
	fileParts := strings.Split(file, string(os.PathSeparator))

	pathSuffix := path.Join(fileParts...)
	if len(fileParts) > 4 {
		pathSuffix = path.Join(fileParts[len(fileParts)-4:]...)
	}

	for _, p := range paths {
		pClean := filepath.Clean(p)
		if pClean == file || pClean == pathSuffix || strings.HasSuffix(pClean, pathSuffix) {
			return true, nil
		}
	}

	return false, nil
}

// ProjectFilesHaveChanges checks if any files related to the compose project have changed.
func ProjectFilesHaveChanges(changedFiles []gitInternal.ChangedFile, project *types.Project, absWorkingDir string) ([]string, error) {
	checks := []struct {
		name string
		fn   func([]gitInternal.ChangedFile, *types.Project, string) (bool, error)
	}{
		{"configs", HasChangedConfigs},
		{"secrets", HasChangedSecrets},
		{"bindMounts", HasChangedBindMounts},
		{"envFiles", HasChangedEnvFiles},
		{"buildFiles", HasChangedBuildFiles},
	}

	var changeReasons []string

	for _, check := range checks {
		changed, err := check.fn(changedFiles, project, absWorkingDir)
		if err != nil {
			return nil, fmt.Errorf("failed to check '%s' for changes: %w", check.name, err)
		}

		if changed {
			changeReasons = append(changeReasons, check.name)
		}
	}

	return changeReasons, nil
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

// ProjectHash generates a SHA256 hash of the project configuration to be used for detecting changes in the project that may require a redeployment.
func ProjectHash(p *types.Project) string {
	b, err := json.Marshal(p)
	if err != nil {
		slog.Error("failed to marshal project for hashing", logger.ErrAttr(err))
		return ""
	}

	return digest.SHA256.FromBytes(b).Encoded()
}
