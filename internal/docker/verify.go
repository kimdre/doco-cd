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
	"strings"
)

var (
	ErrDockerSocketConnectionFailed = errors.New("failed to connect to docker socket")
	ErrDockerHostConnectionFailed   = errors.New("failed to connect to docker host")
)

// ConnectToSocket connects to the docker socket.
func ConnectToSocket() (net.Conn, error) {
	c, err := net.Dial("unix", SocketPath)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func NewHttpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", SocketPath)
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

	resp, err := httpClient.Do(req) // #nosec G704
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
	if _, err := os.Stat(SocketPath); errors.Is(err, os.ErrNotExist) {
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

// VerifyDockerHostConnection verifies the connection to the specified DOCKER_HOST.
func VerifyDockerHostConnection(dockerHost string) error {
	var (
		httpClient *http.Client
		url        string
	)

	switch {
	case strings.HasPrefix(dockerHost, "unix://"):
		socket := strings.TrimPrefix(dockerHost, "unix://")
		httpClient = &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socket)
				},
			},
		}
		url = "http://localhost/info"
	case strings.HasPrefix(dockerHost, "tcp://"):
		addr := strings.TrimPrefix(dockerHost, "tcp://")
		httpClient = &http.Client{}
		url = fmt.Sprintf("http://%s/info", addr)
	default:
		return fmt.Errorf("unsupported DOCKER_HOST scheme: %s", dockerHost)
	}

	req, err := http.NewRequest("GET", url, nil) // #nosec G704
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req) // #nosec G704
	if err != nil {
		return fmt.Errorf("failed to connect to docker host: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to get info: %s", body)
	}

	return nil
}

// VerifyDockerAPIAccess verifies access to the Docker API either via DOCKER_HOST or the default socket.
func VerifyDockerAPIAccess() (error, error) {
	dockerHost := os.Getenv("DOCKER_HOST")
	if dockerHost != "" {
		return VerifyDockerHostConnection(dockerHost), ErrDockerHostConnectionFailed
	}

	return VerifySocketConnection(), ErrDockerSocketConnectionFailed
}
