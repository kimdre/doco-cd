package docker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/module"
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

func CreateDockerCli(quiet bool) (command.Cli, error) {
	return CreateDockerCliWithContext(quiet, "")
}

func CreateDockerCliWithContext(quiet bool, dockerContext string) (command.Cli, error) {
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

	// Capture all writes to cli.Err() in a buffer so that the error printed by
	// DockerEndpoint() (see below) is retrievable regardless of quiet mode.
	var initErrBuf bytes.Buffer

	dockerCli, err := command.NewDockerCli(
		command.WithOutputStream(outputStream),
		command.WithErrorStream(io.MultiWriter(errorStream, &initErrBuf)),
		command.WithAPIClientOptions(client.FromEnv),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker cli: %w", err)
	}

	contextName := strings.TrimSpace(dockerContext)
	if contextName == "" {
		contextName = "default"
	}

	opts := &flags.ClientOptions{Context: contextName, LogLevel: "error"}

	err = dockerCli.Initialize(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize docker cli: %w", err)
	}

	// Discard any non-fatal warnings written by Initialize() (e.g. "WARNING: Error
	// loading config file: permission denied") so they don't trip the check below.
	initErrBuf.Reset()

	/* DockerEndpoint() safely triggers the internal lazy initialization (sync.Once).
	Unlike Client(), it does NOT call os.Exit(1) on failure. Instead, it prints the
	error to cli.Err() (captured above in initErrBuf) and returns whatever partial
	endpoint was resolved. Check the buffer first so we surface the real Docker error. */
	_ = dockerCli.DockerEndpoint()

	if initErrBuf.Len() > 0 {
		return nil, fmt.Errorf("failed to initialize Docker CLI for context %q: %s",
			contextName, strings.TrimSpace(initErrBuf.String()))
	}

	// Wrap so ConfigFile() re-reads the docker config from disk on every auth
	// lookup. This is required for short-lived registry tokens (ECR etc.) that get
	// refreshed on disk while the daemon runs. See reloadConfigCli.
	return reloadConfigCli{dockerCli}, nil
}
