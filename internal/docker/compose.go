package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/notification"

	"github.com/go-git/go-git/v5/plumbing/format/diff"

	"github.com/docker/docker/client"

	gitInternal "github.com/kimdre/doco-cd/internal/git"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/webhook"
)

const (
	socketPath = "/var/run/docker.sock"
)

var (
	ErrDockerSocketConnectionFailed = errors.New("failed to connect to docker socket")
	ErrNoContainerToStart           = errors.New("no container to start")
	ErrIsInUse                      = errors.New("is in use")
	ComposeVersion                  string // Version of the docker compose module, will be set at runtime
)

// ConnectToSocket connects to the docker socket.
func ConnectToSocket() (net.Conn, error) {
	c, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func NewHttpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}
}

// VerifySocketRead verifies whether the application can read from the docker socket.
func VerifySocketRead(httpClient *http.Client) error {
	reqBody, err := json.Marshal("")
	if err != nil {
		return err
	}

	req, err := http.NewRequest("GET", "http://localhost/info", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}

	defer resp.Body.Close() //nolint:errcheck

	responseBody, _ := io.ReadAll(resp.Body)

	// Check for a successful response
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to get containers: %s", responseBody)
	}

	return nil
}

// VerifySocketConnection verifies whether the application can connect to the docker socket.
func VerifySocketConnection() error {
	// Check if the docker socket file exists
	if _, err := os.Stat(socketPath); errors.Is(err, os.ErrNotExist) {
		return err
	}

	c, err := ConnectToSocket()
	if err != nil {
		return err
	}

	httpClient := NewHttpClient()
	defer httpClient.CloseIdleConnections()

	err = VerifySocketRead(httpClient)
	if err != nil {
		return err
	}

	err = c.Close()
	if err != nil {
		return err
	}

	return nil
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
func addComposeServiceLabels(project *types.Project, deployConfig config.DeployConfig, payload webhook.ParsedPayload, repoDir, appVersion, timestamp, composeVersion, latestCommit string) {
	for i, s := range project.Services {
		s.CustomLabels = map[string]string{
			DocoCDLabels.Metadata.Manager:      config.AppName,
			DocoCDLabels.Metadata.Version:      appVersion,
			DocoCDLabels.Deployment.Name:       deployConfig.Name,
			DocoCDLabels.Deployment.Timestamp:  timestamp,
			DocoCDLabels.Deployment.WorkingDir: repoDir,
			DocoCDLabels.Deployment.Trigger:    payload.CommitSHA,
			DocoCDLabels.Deployment.CommitSHA:  latestCommit,
			DocoCDLabels.Deployment.TargetRef:  deployConfig.Reference,
			DocoCDLabels.Repository.Name:       payload.FullName,
			DocoCDLabels.Repository.URL:        payload.WebURL,
			api.ProjectLabel:                   project.Name,
			api.ServiceLabel:                   s.Name,
			api.WorkingDirLabel:                project.WorkingDir,
			api.ConfigFilesLabel:               strings.Join(project.ComposeFiles, ","),
			api.VersionLabel:                   composeVersion,
			api.OneoffLabel:                    "False", // default, will be overridden by docker compose
		}
		project.Services[i] = s
	}
}

func addComposeVolumeLabels(project *types.Project, deployConfig config.DeployConfig, payload webhook.ParsedPayload, appVersion, timestamp, composeVersion, latestCommit string) {
	for i, v := range project.Volumes {
		v.CustomLabels = map[string]string{
			DocoCDLabels.Metadata.Manager:     config.AppName,
			DocoCDLabels.Metadata.Version:     appVersion,
			DocoCDLabels.Deployment.Name:      deployConfig.Name,
			DocoCDLabels.Deployment.Timestamp: timestamp,
			DocoCDLabels.Deployment.Trigger:   payload.CommitSHA,
			DocoCDLabels.Deployment.TargetRef: deployConfig.Reference,
			DocoCDLabels.Deployment.CommitSHA: latestCommit,
			DocoCDLabels.Repository.Name:      payload.FullName,
			DocoCDLabels.Repository.URL:       payload.WebURL,
			api.ProjectLabel:                  project.Name,
			api.VolumeLabel:                   v.Name,
			api.VersionLabel:                  composeVersion,
		}
		project.Volumes[i] = v
	}
}

// LoadCompose parses and loads Compose files as specified by the Docker Compose specification.
func LoadCompose(ctx context.Context, workingDir, projectName string, composeFiles, profiles []string) (*types.Project, error) {
	options, err := cli.NewProjectOptions(
		composeFiles,
		cli.WithName(projectName),
		cli.WithWorkingDirectory(workingDir),
		cli.WithInterpolation(true),
		cli.WithResolvedPaths(true),
		cli.WithEnvFiles(),
		cli.WithProfiles(profiles),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create project options: %w", err)
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
	repoDir, latestCommit, appVersion string, forceDeploy bool,
) error {
	var err error

	service := compose.NewComposeService(dockerCli)

	timestamp := time.Now().UTC().Format(time.RFC3339)

	if ComposeVersion == "" {
		ComposeVersion, err = GetModuleVersion("github.com/docker/compose/v2")
		if err != nil {
			if errors.Is(err, ErrModuleNotFound) {
				// Placeholder for when the module is not found
				ComposeVersion = "unknown"
			} else {
				return fmt.Errorf("failed to get module version: %w", err)
			}
		}
	}

	addComposeServiceLabels(project, *deployConfig, payload, repoDir, appVersion, timestamp, ComposeVersion, latestCommit)
	addComposeVolumeLabels(project, *deployConfig, payload, appVersion, timestamp, ComposeVersion, latestCommit)

	if deployConfig.ForceImagePull {
		err := service.Pull(ctx, project, api.PullOptions{
			Quiet: true,
		})
		if err != nil {
			return err
		}
	}

	recreateType := api.RecreateDiverged
	if deployConfig.ForceRecreate || forceDeploy {
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

	return nil
}

// DeployStack deploys the stack using the provided deployment configuration.
func DeployStack(
	jobLog *slog.Logger, internalRepoPath, externalRepoPath string, ctx *context.Context,
	dockerCli *command.Cli, dockerClient *client.Client, payload *webhook.ParsedPayload, deployConfig *config.DeployConfig,
	changedFiles []gitInternal.ChangedFile, latestCommit, appVersion, triggerEvent string, forceDeploy bool,
	metadata notification.Metadata,
) error {
	startTime := time.Now()

	stackLog := jobLog.
		With(slog.String("stack", deployConfig.Name))

	// Validate and sanitize the working directory
	if strings.Contains(deployConfig.WorkingDirectory, "..") {
		errMsg := "invalid working directory: potential path traversal detected"
		jobLog.Error(errMsg, slog.String("working_directory", deployConfig.WorkingDirectory))

		return fmt.Errorf("%s: %w", errMsg, errors.New("validation error"))
	}

	// Path inside the container
	internalWorkingDir := path.Join(internalRepoPath, deployConfig.WorkingDirectory)

	internalWorkingDir, err := filepath.Abs(internalWorkingDir)
	if err != nil || !strings.HasPrefix(internalWorkingDir, internalRepoPath) {
		errMsg := "invalid working directory: resolved path is outside the allowed base directory"
		jobLog.Error(errMsg, slog.String("resolved_path", internalWorkingDir))

		return fmt.Errorf("%s", errMsg)
	}

	// Path on the host
	externalWorkingDir := path.Join(externalRepoPath, deployConfig.WorkingDirectory)

	externalWorkingDir, err = filepath.Abs(externalWorkingDir)
	if err != nil || !strings.HasPrefix(externalWorkingDir, externalRepoPath) {
		errMsg := "invalid working directory: resolved path is outside the allowed base directory"
		jobLog.Error(errMsg, slog.String("resolved_path", externalWorkingDir))

		return fmt.Errorf("%s", errMsg)
	}

	err = os.Chdir(internalWorkingDir)
	if err != nil {
		errMsg := "failed to change internal working directory"
		jobLog.Error(errMsg, logger.ErrAttr(err), slog.String("path", internalWorkingDir))

		return fmt.Errorf("%s: %w", errMsg, err)
	}

	// Check if the default compose files are used
	if reflect.DeepEqual(deployConfig.ComposeFiles, cli.DefaultFileNames) {
		var tmpComposeFiles []string

		jobLog.Debug("checking for default compose files")

		// Check if the default compose files exist
		for _, f := range deployConfig.ComposeFiles {
			if _, err = os.Stat(path.Join(internalWorkingDir, f)); errors.Is(err, os.ErrNotExist) {
				continue
			}

			tmpComposeFiles = append(tmpComposeFiles, f)
		}

		if len(tmpComposeFiles) == 0 {
			errMsg := "no compose files found"
			stackLog.Error(errMsg,
				slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

			return fmt.Errorf("%s: %w", errMsg, err)
		}

		deployConfig.ComposeFiles = tmpComposeFiles
	}

	// Check if files in the working directory are SOPS encrypted and decrypt them if necessary
	err = filepath.WalkDir(internalWorkingDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("failed to walk directory %s: %w", path, err)
		}

		dirPath := filepath.Dir(path)
		dirName := filepath.Base(dirPath)

		// Check if dirPath is part of the paths to ignore
		if slices.Contains(encryption.IgnoreDirs, dirName) {
			stackLog.Debug("skipping directory", slog.String("path", dirPath), slog.String("ignore_path", dirName))

			return filepath.SkipDir
		}

		if d.IsDir() {
			return nil
		}

		isEncrypted, err := encryption.IsEncryptedFile(path)
		if err != nil {
			return fmt.Errorf("failed to check if file is encrypted: %w", err)
		}

		if isEncrypted {
			if !encryption.SopsKeyIsSet() {
				return fmt.Errorf("SOPS secret key is not set, cannot decrypt file: %s", path)
			}

			stackLog.Debug("encrypted file detected, decrypting", slog.String("file", path))

			decryptedContent, err := encryption.DecryptFile(path)
			if err != nil {
				return fmt.Errorf("failed to decrypt file %s: %w", path, err)
			}

			err = os.WriteFile(path, decryptedContent, filesystem.PermOwner)
			if err != nil {
				return fmt.Errorf("failed to write decrypted content to file %s: %w", path, err)
			}

			stackLog.Debug("file decrypted successfully", slog.String("file", path))
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("file decryption failed: %w", err)
	}

	project, err := LoadCompose(*ctx, externalWorkingDir, deployConfig.Name, deployConfig.ComposeFiles, deployConfig.Profiles)
	if err != nil {
		errMsg := "failed to load compose config"
		stackLog.Error(errMsg, logger.ErrAttr(err), slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

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
	if SwarmModeEnabled {
		// Check if the project has bind mounts with swarm mode and fail if it does.
		for _, service := range project.Services {
			for _, volume := range service.Volumes {
				if volume.Type == "bind" {
					prometheus.DeploymentErrorsTotal.WithLabelValues(deployConfig.Name).Inc()

					errMsg := "swarm mode does not support bind mounts, please use volumes, configs or secrets instead"

					return fmt.Errorf("%s: service: %s", errMsg, service.Name)
				}
			}
		}

		stackLog.Info("deploying swarm stack")

		err = DeploySwarmStack(*ctx, *dockerCli, project, deployConfig, *payload, externalWorkingDir, latestCommit, appVersion)
		if err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(deployConfig.Name).Inc()

			errMsg := "failed to deploy swarm stack"
			stackLog.Error(errMsg, logger.ErrAttr(err),
				slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

			return fmt.Errorf("%s: %w", errMsg, err)
		}

		err = PruneStackConfigs(*ctx, dockerClient, deployConfig.Name)
		if err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(deployConfig.Name).Inc()

			errMsg := "failed to prune stack configs"
			stackLog.Error(errMsg, logger.ErrAttr(err),
				slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

			return fmt.Errorf("%s: %w", errMsg, err)
		}

		err = PruneStackSecrets(*ctx, dockerClient, deployConfig.Name)
		if err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(deployConfig.Name).Inc()

			errMsg := "failed to prune stack secrets"
			stackLog.Error(errMsg, logger.ErrAttr(err),
				slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

			return fmt.Errorf("%s: %w", errMsg, err)
		}
	} else {
		hasChangedFiles, err := ProjectFilesHaveChanges(changedFiles, project)
		if err != nil {
			errMsg := "failed to check for changed project files"
			stackLog.Error(errMsg, logger.ErrAttr(err), slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

			return fmt.Errorf("%s: %w", errMsg, err)
		}

		hasChangedCompose, err := HasChangedComposeFiles(changedFiles, project)
		if err != nil {
			errMsg := "failed to check for changed compose files"
			stackLog.Error(errMsg, logger.ErrAttr(err), slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

			return fmt.Errorf("%s: %w", errMsg, err)
		}

		switch {
		case hasChangedFiles || (hasChangedCompose && triggerEvent == "poll"):
			deployConfig.ForceRecreate = true

			stackLog.Debug("changed mounted files detected, forcing recreate of all services")
		case hasChangedCompose:
			stackLog.Debug("changed compose files detected, continue normal deployment")
		}

		stackLog.Info("deploying stack", slog.Bool("forced", deployConfig.ForceRecreate))

		err = deployCompose(*ctx, *dockerCli, project, deployConfig, *payload, externalWorkingDir, latestCommit, appVersion, forceDeploy)
		if err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(deployConfig.Name).Inc()

			errMsg := "failed to deploy stack"
			stackLog.Error(errMsg, logger.ErrAttr(err),
				slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

			return fmt.Errorf("%s: %w", errMsg, err)
		}
	}

	prometheus.DeploymentsTotal.WithLabelValues(deployConfig.Name).Inc()
	prometheus.DeploymentDuration.WithLabelValues(deployConfig.Name).Observe(time.Since(startTime).Seconds())

	msg := "successfully deployed stack " + deployConfig.Name

	err = notification.Send(notification.Success, "Deployment Successful", msg, metadata)
	if err != nil {
		return err
	}

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

	if SwarmModeEnabled {
		err := RemoveSwarmStack(*ctx, *dockerCli, deployConfig)
		if err != nil {
			errMsg := "failed to destroy swarm stack"
			stackLog.Error(errMsg, logger.ErrAttr(err))

			return fmt.Errorf("%s: %w", errMsg, err)
		}

		return nil
	}

	service := compose.NewComposeService(*dockerCli)

	downOpts := api.DownOptions{
		RemoveOrphans: deployConfig.RemoveOrphans,
		Volumes:       deployConfig.DestroyOpts.RemoveVolumes,
	}

	if deployConfig.DestroyOpts.RemoveImages {
		downOpts.Images = "all"
	}

	err := service.Down(*ctx, deployConfig.Name, downOpts)
	if err != nil {
		errMsg := "failed to destroy stack"
		stackLog.Error(errMsg, logger.ErrAttr(err))

		return fmt.Errorf("%s: %w", errMsg, err)
	}

	return nil
}

func getAbsolutePaths(changedFiles []gitInternal.ChangedFile, workingDir string) []string {
	var absPaths []string

	for _, f := range changedFiles {
		checkPaths := []diff.File{f.From, f.To}

		for _, checkPath := range checkPaths {
			if checkPath == nil {
				continue
			}

			p := filepath.Clean(checkPath.Path())

			if !filepath.IsAbs(p) {
				w := filepath.Clean(workingDir)

				for {
					if strings.HasPrefix(p, filepath.Base(w)) {
						w = filepath.Dir(w)
					} else {
						p = filepath.Join(w, p)
						break
					}
				}
			}

			if !slices.Contains(absPaths, p) {
				absPaths = append(absPaths, p)
			}
		}
	}

	return absPaths
}

// HasChangedConfigs checks if any files used in docker compose `configs:` definitions have changed using the Git status.
func HasChangedConfigs(changedFiles []gitInternal.ChangedFile, project *types.Project) (bool, error) {
	paths := getAbsolutePaths(changedFiles, project.WorkingDir)

	for _, c := range project.Configs {
		configPath := c.File
		if configPath == "" {
			continue
		}

		if !path.IsAbs(configPath) {
			configPath = filepath.Join(project.WorkingDir, configPath)
		}

		for _, p := range paths {
			if p == configPath {
				return true, nil
			}
		}
	}

	return false, nil
}

// HasChangedSecrets checks if any files used in docker compose `secrets:` definitions have changed using the Git status.
func HasChangedSecrets(changedFiles []gitInternal.ChangedFile, project *types.Project) (bool, error) {
	paths := getAbsolutePaths(changedFiles, project.WorkingDir)

	for _, s := range project.Secrets {
		secretPath := s.File
		if secretPath == "" {
			continue
		}

		if !path.IsAbs(secretPath) {
			secretPath = filepath.Join(project.WorkingDir, secretPath)
		}

		for _, p := range paths {
			if p == secretPath {
				return true, nil
			}
		}
	}

	return false, nil
}

// HasChangedBindMounts checks if any files used in docker compose `volumes:` definitions with type `bind` have changed using the Git status.
func HasChangedBindMounts(changedFiles []gitInternal.ChangedFile, project *types.Project) (bool, error) {
	paths := getAbsolutePaths(changedFiles, project.WorkingDir)

	for _, s := range project.Services {
		for _, v := range s.Volumes {
			if v.Type == "bind" && v.Source != "" {
				for _, p := range paths {
					info, err := os.Stat(p)
					if err != nil {
						if errors.Is(err, os.ErrNotExist) {
							continue
						}

						return false, fmt.Errorf("failed to stat bind mount source %s: %w", p, err)
					}

					// Redeployment is not needed if the bind mount source is a directory
					if info.IsDir() {
						return false, nil
					}

					if strings.HasPrefix(p, v.Source) {
						return true, nil
					}
				}
			}
		}
	}

	return false, nil
}

// HasChangedEnvFiles checks if any files used in docker compose `env_file:` definitions have changed using the Git status.
func HasChangedEnvFiles(changedFiles []gitInternal.ChangedFile, project *types.Project) (bool, error) {
	paths := getAbsolutePaths(changedFiles, project.WorkingDir)

	for _, s := range project.Services {
		for _, envFile := range s.EnvFiles {
			envFilePath := envFile.Path

			if !path.IsAbs(envFilePath) {
				envFilePath = filepath.Join(project.WorkingDir, envFilePath)
			}

			for _, p := range paths {
				if p == envFilePath {
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
	paths := getAbsolutePaths(changedFiles, project.WorkingDir)

	for _, composeFile := range project.ComposeFiles {
		if !path.IsAbs(composeFile) {
			composeFile = filepath.Join(project.WorkingDir, composeFile)
		}

		// Get the last 4 parts of the composeFile path
		composeFileParts := strings.Split(composeFile, string(os.PathSeparator))

		pathSuffix := path.Join(composeFileParts...)
		if len(composeFileParts) > 4 {
			pathSuffix = path.Join(composeFileParts[len(composeFileParts)-4:]...)
		}

		for _, p := range paths {
			if strings.HasSuffix(p, pathSuffix) {
				return true, nil
			}
		}
	}

	return false, nil
}

// ProjectFilesHaveChanges checks if any files related to the compose project have changed.
func ProjectFilesHaveChanges(changedFiles []gitInternal.ChangedFile, project *types.Project) (bool, error) {
	checks := map[string]func([]gitInternal.ChangedFile, *types.Project) (bool, error){
		"configs":    HasChangedConfigs,
		"secrets":    HasChangedSecrets,
		"bindMounts": HasChangedBindMounts,
		"envFiles":   HasChangedEnvFiles,
	}

	for name, check := range checks {
		hasChanges, err := check(changedFiles, project)
		if err != nil {
			return false, fmt.Errorf("failed to check %s for changes: %w", name, err)
		}

		if hasChanges {
			return true, nil
		}
	}

	return false, nil
}
