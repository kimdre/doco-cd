package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kimdre/doco-cd/internal/webhook"

	"github.com/kimdre/doco-cd/internal/config"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
)

const (
	socketPath = "/var/run/docker.sock"
	baseLabel  = "doco"
)

var (
	ErrDockerSocketConnectionFailed = errors.New("failed to connect to docker socket")
	ErrNoContainerToStart           = errors.New("no container to start")
)

// ConnectToSocket connects to the docker socket
func ConnectToSocket() (net.Conn, error) {
	c, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func GetSocketGroupOwner() (string, error) {
	fi, err := os.Stat(socketPath)
	if err != nil {
		return "", err
	}

	return strconv.Itoa(int(fi.Sys().(*syscall.Stat_t).Gid)), nil
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

	defer resp.Body.Close()

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
	if _, err := os.Stat("/var/run/docker.sock"); errors.Is(err, os.ErrNotExist) {
		return err
	}

	c, err := ConnectToSocket()

	// If ErrPermissionDenied is returned, return the required permissions
	if errors.Is(err, os.ErrPermission) {
		gid, err := GetSocketGroupOwner()
		if err != nil {
			return err
		}

		return fmt.Errorf("%v: current user needs group id %v", os.ErrPermission, gid)
	} else if err != nil {
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
func addServiceLabels(project *types.Project, payload webhook.ParsedPayload) {
	for i, s := range project.Services {
		s.CustomLabels = map[string]string{
			"cd.doco.deployedAt":           time.Now().UTC().Format(time.RFC3339),
			"cd.doco.repository.name":      payload.FullName,
			"cd.doco.repository.private":   strconv.FormatBool(payload.Private),
			"cd.doco.repository.reference": payload.Ref,
			"cd.doco.repository.commit":    payload.CommitSHA,
			api.ProjectLabel:               project.Name,
			api.ServiceLabel:               s.Name,
			api.VersionLabel:               api.ComposeVersion,
			api.WorkingDirLabel:            project.WorkingDir,
			api.ConfigFilesLabel:           strings.Join(project.ComposeFiles, ","),
			api.OneoffLabel:                "False", // default, will be overridden by `run` command
		}
		project.Services[i] = s
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
	)
	if err != nil {
		return nil, err
	}

	project, err := options.LoadProject(ctx)
	if err != nil {
		return nil, err
	}

	return project, nil
}

// DeployCompose deploys a project as specified by the Docker Compose specification (LoadCompose)
func DeployCompose(ctx context.Context, dockerCli command.Cli, project *types.Project, deployConfig *config.DeployConfig, payload webhook.ParsedPayload) error {
	service := compose.NewComposeService(dockerCli)

	addServiceLabels(project, payload)

	if deployConfig.ForceImagePull {
		err := service.Pull(ctx, project, api.PullOptions{
			Quiet: true,
		})
		if err != nil {
			return err
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

	err := service.Build(ctx, project, buildOpts)
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
