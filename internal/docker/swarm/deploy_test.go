package swarm

import (
	"fmt"
	"os"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
)

func TestCheckDaemonIsSwarmManager(t *testing.T) {
	dockerCli, err := command.NewDockerCli(
		command.WithOutputStream(os.Stdout),
		command.WithErrorStream(os.Stderr),
	)
	if err != nil {
		t.Fatalf("Failed to create docker cli: %v", err)
	}

	opts := &flags.ClientOptions{Context: "default", LogLevel: "error", TLSVerify: false}

	err = dockerCli.Initialize(opts)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to initialize docker cli: %w", err))
	}

	_, err = CheckDaemonIsSwarmManager(t.Context(), dockerCli)
	if err != nil {
		t.Fatalf("Failed to check if Docker daemon is a swarm manager: %v", err)
	}

	t.Logf("Docker daemon is a swarm manager: %v", err == nil)
}
