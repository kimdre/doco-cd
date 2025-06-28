package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/encryption"

	"github.com/kimdre/doco-cd/internal/logger"

	"github.com/kimdre/doco-cd/internal/utils"

	"github.com/kimdre/doco-cd/internal/webhook"

	"github.com/kimdre/doco-cd/internal/config"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"

	"github.com/compose-spec/compose-go/v2/cli"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
)

const (
	socketPath = "/var/run/docker.sock"
)

var (
	ErrDockerSocketConnectionFailed = errors.New("failed to connect to docker socket")
	ErrNoContainerToStart           = errors.New("no container to start")
	ComposeVersion                  string
)

// ConnectToSocket connects to the docker socket
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
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}
}

// VerifySocketRead verifies whether the application can read from the docker socket
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

// VerifySocketConnection verifies whether the application can connect to the docker socket
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
addServiceLabels adds the labels docker compose expects to exist on services.
This is required for future compose operations to work, such as finding
containers that are part of a service.
*/
func addServiceLabels(project *types.Project, deployConfig config.DeployConfig, payload webhook.ParsedPayload, repoDir, appVersion, timestamp, composeVersion, latestCommit string) {
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

func addVolumeLabels(project *types.Project, deployConfig config.DeployConfig, payload webhook.ParsedPayload, appVersion, timestamp, composeVersion, latestCommit string) {
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

// LoadCompose parses and loads Compose files as specified by the Docker Compose specification
func LoadCompose(ctx context.Context, workingDir, projectName string, composeFiles []string) (*types.Project, error) {
	options, err := cli.NewProjectOptions(
		composeFiles,
		cli.WithName(projectName),
		cli.WithWorkingDirectory(workingDir),
		cli.WithInterpolation(true),
		cli.WithResolvedPaths(true),
		cli.WithDotEnv,
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

// DeployCompose deploys a project as specified by the Docker Compose specification (LoadCompose)
func DeployCompose(ctx context.Context, dockerCli command.Cli, project *types.Project,
	deployConfig *config.DeployConfig, payload webhook.ParsedPayload,
	repoDir, latestCommit, appVersion string, forceDeploy bool,
) error {
	var err error

	service := compose.NewComposeService(dockerCli)

	timestamp := time.Now().UTC().Format(time.RFC3339)

	if ComposeVersion == "" {
		ComposeVersion, err = utils.GetModuleVersion("github.com/docker/compose/v2")
		if err != nil {
			if errors.Is(err, utils.ErrModuleNotFound) {
				// Placeholder for when the module is not found
				ComposeVersion = "unknown"
			} else {
				return fmt.Errorf("failed to get module version: %w", err)
			}
		}
	}

	addServiceLabels(project, *deployConfig, payload, repoDir, appVersion, timestamp, ComposeVersion, latestCommit)
	addVolumeLabels(project, *deployConfig, payload, appVersion, timestamp, ComposeVersion, latestCommit)

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

// DeployStack deploys the stack using the provided deployment configuration
func DeployStack(
	jobLog *slog.Logger, internalRepoPath, externalRepoPath string, ctx *context.Context,
	dockerCli *command.Cli, payload *webhook.ParsedPayload, deployConfig *config.DeployConfig,
	latestCommit, appVersion string, forceDeploy bool,
) error {
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

	if encryption.SopsKeyIsSet {
		// Check if files in the working directory are SOPS encrypted
		files, _ := os.ReadDir(internalWorkingDir)
		for _, file := range files {
			if file.IsDir() {
				continue
			}

			p := filepath.Join(internalWorkingDir, file.Name())

			isEncrypted, err := encryption.IsSopsEncryptedFile(p)
			if err != nil {
				return err
			}

			if isEncrypted {
				// TODO: Change this to Debug level
				stackLog.Info("SOPS encrypted file detected, decrypting",
					slog.String("file", file.Name()),
					slog.String("working_directory", deployConfig.WorkingDirectory))

				decryptedContent, err := encryption.DecryptSopsFile(p)
				if err != nil {
					errMsg := "failed to decrypt SOPS file"
					stackLog.Error(errMsg,
						logger.ErrAttr(err),
						slog.String("file", file.Name()))

					return fmt.Errorf("%s: %w", errMsg, err)
				}

				err = os.WriteFile(p, decryptedContent, 0o644)
				if err != nil {
					errMsg := "failed to write decrypted content to file"
					stackLog.Error(errMsg,
						logger.ErrAttr(err),
						slog.String("file", file.Name()))

					return fmt.Errorf("%s: %w", errMsg, err)
				}
			}
		}
	}

	project, err := LoadCompose(*ctx, externalWorkingDir, deployConfig.Name, deployConfig.ComposeFiles)
	if err != nil {
		errMsg := "failed to load compose config"
		stackLog.Error(errMsg,
			logger.ErrAttr(err),
			slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

		return fmt.Errorf("%s: %w", errMsg, err)
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

	err = DeployCompose(*ctx, *dockerCli, project, deployConfig, *payload, externalWorkingDir, latestCommit, appVersion, forceDeploy)
	if err != nil {
		errMsg := "failed to deploy stack"
		stackLog.Error(errMsg,
			logger.ErrAttr(err),
			slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

		return fmt.Errorf("%s: %w", errMsg, err)
	}

	return nil
}

// DestroyStack destroys the stack using the provided deployment configuration
func DestroyStack(
	jobLog *slog.Logger, ctx *context.Context,
	dockerCli *command.Cli, deployConfig *config.DeployConfig,
) error {
	stackLog := jobLog.
		With(slog.String("stack", deployConfig.Name))

	stackLog.Info("destroying stack")

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
		stackLog.Error(errMsg,
			logger.ErrAttr(err),
		)

		return fmt.Errorf("%s: %w", errMsg, err)
	}

	return nil
}
