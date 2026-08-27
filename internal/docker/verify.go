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
	return ConnectToSocketContext(context.Background())
}

// ConnectToSocketContext connects to the Docker socket with context cancellation.
func ConnectToSocketContext(ctx context.Context) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", SocketPath)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func NewHttpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", SocketPath)
			},
		},
	}
}

// VerifySocketRead verifies whether the application can read from the docker socket.
func VerifySocketRead(httpClient *http.Client) error {
	return VerifySocketReadContext(context.Background(), httpClient)
}

// VerifySocketReadContext verifies whether the application can read from the Docker socket with context cancellation.
func VerifySocketReadContext(ctx context.Context, httpClient *http.Client) error {
	reqBody, err := json.Marshal("")
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/info", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req) // #nosec G704
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to get containers: %s", responseBody)
	}

	return nil
}

// VerifySocketConnection verifies whether the application can connect to the docker socket.
func VerifySocketConnection() error {
	return VerifySocketConnectionContext(context.Background())
}

// VerifySocketConnectionContext verifies whether the application can connect to the Docker socket with context cancellation.
func VerifySocketConnectionContext(ctx context.Context) error {
	if _, err := os.Stat(SocketPath); errors.Is(err, os.ErrNotExist) {
		return err
	}

	c, err := ConnectToSocketContext(ctx)
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck

	httpClient := NewHttpClient()
	defer httpClient.CloseIdleConnections()

	return VerifySocketReadContext(ctx, httpClient)
}

// VerifyDockerHostConnection verifies the connection to the specified DOCKER_HOST.
func VerifyDockerHostConnection(dockerHost string) error {
	return VerifyDockerHostConnectionContext(context.Background(), dockerHost)
}

// VerifyDockerHostConnectionContext verifies the connection to the specified DOCKER_HOST with context cancellation.
func VerifyDockerHostConnectionContext(ctx context.Context, dockerHost string) error {
	var (
		httpClient *http.Client
		url        string
	)

	switch {
	case strings.HasPrefix(dockerHost, "unix://"):
		socket := strings.TrimPrefix(dockerHost, "unix://")
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket) // #nosec G704
			},
		}
		httpClient = &http.Client{Transport: transport}
		url = "http://localhost/info"
	case strings.HasPrefix(dockerHost, "tcp://"):
		addr := strings.TrimPrefix(dockerHost, "tcp://")
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", addr) // #nosec G704
			},
		}
		httpClient = &http.Client{Transport: transport}
		url = fmt.Sprintf("http://%s/info", addr)
	default:
		return fmt.Errorf("unsupported DOCKER_HOST scheme: %s", dockerHost)
	}

	defer httpClient.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) // #nosec G704
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
	return VerifyDockerAPIAccessContext(context.Background())
}

// VerifyDockerAPIAccessContext verifies access to the Docker API with context cancellation.
func VerifyDockerAPIAccessContext(ctx context.Context) (error, error) {
	dockerHost := os.Getenv("DOCKER_HOST")
	if dockerHost != "" {
		return VerifyDockerHostConnectionContext(ctx, dockerHost), ErrDockerHostConnectionFailed
	}

	return VerifySocketConnectionContext(ctx), ErrDockerSocketConnectionFailed
}
