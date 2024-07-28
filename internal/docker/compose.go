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
	"syscall"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
)

const (
	socketPath = "/var/run/docker.sock"
	apiVersion = "v1.46"
)

var ErrDockerSocketConnectionFailed = errors.New("failed to connect to docker socket")

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

	req, err := http.NewRequest("GET", fmt.Sprintf("http://localhost/%s/info", apiVersion), bytes.NewReader(reqBody))
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

// LoadCompose parses and loads Compose files as specified by the Docker Compose specification
func LoadCompose(ctx context.Context, workingDir, projectName string, composeFiles []string) (*types.Project, error) {
	// Iterate over the compose files and remove the first one until the project is successfully loaded
	// for len(composeFiles) > 0 {
	options, err := cli.NewProjectOptions(
		composeFiles,
		cli.WithName(projectName),
		cli.WithWorkingDirectory(workingDir),
		cli.WithInterpolation(true),
		cli.WithResolvedPaths(true),
	)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	project, err := options.LoadProject(ctx)
	if err != nil {
		return nil, err
	}

	//	}
	//		composeFiles = composeFiles[1:]
	//		// Remove the first compose file
	return project, nil
}

// DeployCompose deploys a project as specified by the Docker Compose specification (LoadCompose)
func DeployCompose(ctx context.Context, project *types.Project) error {
	service := compose.NewComposeService(nil)

	createOpts := api.CreateOptions{
		RemoveOrphans: true,
	}

	startOpts := api.StartOptions{
		Project: project,
	}

	err := service.Up(ctx, project, api.UpOptions{
		Create: createOpts,
		Start:  startOpts,
	})
	if err != nil {
		return fmt.Errorf("failed to deploy compose project: %w", err)
	}

	return nil
}
