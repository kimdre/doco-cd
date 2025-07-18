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
	ErrVolumeIsInUse                = errors.New("volume is in use")
	ComposeVersion                  string // Version of the docker compose module, will be set at runtime
	SwarmModeEnabled                bool   // Whether the docker host is running in swarm mode
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
func LoadCompose(ctx context.Context, workingDir, projectName string, composeFiles []string) (*types.Project, error) {
	options, err := cli.NewProjectOptions(
		composeFiles,
		cli.WithName(projectName),
		cli.WithWorkingDirectory(workingDir),
		cli.WithInterpolation(true),
		cli.WithResolvedPaths(true),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create project options: %w", err)
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
	dockerCli *command.Cli, payload *webhook.ParsedPayload, deployConfig *config.DeployConfig,
	changedFiles []gitInternal.ChangedFile, latestCommit, appVersion string, forceDeploy bool,
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

	project, err := LoadCompose(*ctx, externalWorkingDir, deployConfig.Name, deployConfig.ComposeFiles)
	if err != nil {
		errMsg := "failed to load compose config"
		stackLog.Error(errMsg, logger.ErrAttr(err), slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

		return fmt.Errorf("%s: %w", errMsg, err)
	}

	hasChanges, err := MountedFilesHaveChanges(changedFiles, project)
	if err != nil {
		errMsg := "failed to check for changed project files"
		stackLog.Error(errMsg, logger.ErrAttr(err), slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

		return fmt.Errorf("%s: %w", errMsg, err)
	}

	if hasChanges {
		stackLog.Info("mounted files have changed, forcing recreation of the stack")

		deployConfig.ForceRecreate = true
	}

	stackLog.Info("deploying stack")

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
		err = deploySwarmStack(*ctx, *dockerCli, project, deployConfig, *payload, externalWorkingDir, latestCommit, appVersion)
		if err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(deployConfig.Name).Inc()

			errMsg := "failed to deploy swarm stack"
			stackLog.Error(errMsg, logger.ErrAttr(err),
				slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

			return fmt.Errorf("%s: %w", errMsg, err)
		}
	} else {
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
		err := removeSwarmStack(*ctx, *dockerCli, deployConfig)
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

// HasChangedConfigs checks if any files used in docker compose `configs:` definitions have changed using the Git status.
func HasChangedConfigs(changedFiles []gitInternal.ChangedFile, project *types.Project) (bool, error) {
	for _, c := range project.Configs {
		configPath := c.File
		if configPath == "" {
			continue
		}

		if !path.IsAbs(configPath) {
			configPath = filepath.Join(project.WorkingDir, configPath)
		}

		for _, f := range changedFiles {
			var paths []string

			if f.From != nil {
				fromPath := f.From.Path()
				if !path.IsAbs(fromPath) {
					fromPath = filepath.Join(project.WorkingDir, fromPath)
				}

				paths = append(paths, fromPath)
			}

			if f.To != nil {
				toPath := f.To.Path()
				if !path.IsAbs(toPath) {
					toPath = filepath.Join(project.WorkingDir, toPath)
				}

				paths = append(paths, toPath)
			}

			for _, p := range paths {
				if p == configPath {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// HasChangedSecrets checks if any files used in docker compose `secrets:` definitions have changed using the Git status.
func HasChangedSecrets(changedFiles []gitInternal.ChangedFile, project *types.Project) (bool, error) {
	for _, s := range project.Secrets {
		secretPath := s.File
		if secretPath == "" {
			continue
		}

		if !path.IsAbs(secretPath) {
			secretPath = filepath.Join(project.WorkingDir, secretPath)
		}

		for _, f := range changedFiles {
			var paths []string

			if f.From != nil {
				fromPath := f.From.Path()
				if !path.IsAbs(fromPath) {
					fromPath = filepath.Join(project.WorkingDir, fromPath)
				}

				paths = append(paths, fromPath)
			}

			if f.To != nil {
				toPath := f.To.Path()
				if !path.IsAbs(toPath) {
					toPath = filepath.Join(project.WorkingDir, toPath)
				}

				paths = append(paths, toPath)
			}

			for _, p := range paths {
				if p == secretPath {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// HasChangedBindMounts checks if any files used in docker compose `volumes:` definitions with type `bind` have changed using the Git status.
func HasChangedBindMounts(changedFiles []gitInternal.ChangedFile, project *types.Project) (bool, error) {
	for _, s := range project.Services {
		for _, v := range s.Volumes {
			if v.Type == "bind" && v.Source != "" {
				bindPath := v.Source
				if !path.IsAbs(bindPath) {
					bindPath = filepath.Join(project.WorkingDir, bindPath)
				}

				for _, f := range changedFiles {
					var paths []string

					if f.From != nil {
						fromPath := f.From.Path()
						if !path.IsAbs(fromPath) {
							fromPath = filepath.Join(project.WorkingDir, fromPath)
						}

						paths = append(paths, fromPath)
					}

					if f.To != nil {
						toPath := f.To.Path()
						if !path.IsAbs(toPath) {
							toPath = filepath.Join(project.WorkingDir, toPath)
						}

						paths = append(paths, toPath)
					}

					for _, p := range paths {
						// Check if bindPath is in the changed file path
						if strings.HasPrefix(p, bindPath) {
							return true, nil
						}
					}
				}
			}
		}
	}

	return false, nil
}

// MountedFilesHaveChanges checks if any files from config, secret or bind mounts have changed in the project.
func MountedFilesHaveChanges(changedFiles []gitInternal.ChangedFile, project *types.Project) (bool, error) {
	changedConfigs, err := HasChangedConfigs(changedFiles, project)
	if err != nil {
		return false, fmt.Errorf("failed to check changed configs: %w", err)
	}

	if changedConfigs {
		return true, nil
	}

	changedSecrets, err := HasChangedSecrets(changedFiles, project)
	if err != nil {
		return false, fmt.Errorf("failed to check changed secrets: %w", err)
	}

	if changedSecrets {
		return true, nil
	}

	changedBindMounts, err := HasChangedBindMounts(changedFiles, project)
	if err != nil {
		return false, fmt.Errorf("failed to check changed bind mounts: %w", err)
	}

	if changedBindMounts {
		return true, nil
	}

	return false, nil
}
