package test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	composeCli "github.com/compose-spec/compose-go/v2/cli"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// ComposeStack represents a running compose stack for tests.
type ComposeStack struct {
	Name      string
	Service   api.Compose
	DockerCli command.Cli
	Client    *client.Client
}

// NewDockerCli creates a docker CLI for test use.
func NewDockerCli() (command.Cli, error) {
	dockerCli, err := command.NewDockerCli(
		command.WithOutputStream(io.Discard),
		command.WithErrorStream(os.Stderr),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker cli: %w", err)
	}

	opts := &flags.ClientOptions{Context: "default", LogLevel: "error"}
	if err = dockerCli.Initialize(opts); err != nil {
		return nil, fmt.Errorf("failed to initialize docker cli: %w", err)
	}

	return dockerCli, nil
}

// ComposeUp deploys a compose stack from a YAML string using the test name as stack name.
// Cleanup (compose down) is registered automatically via t.Cleanup.
func ComposeUp(ctx context.Context, t *testing.T, composeYAML string) *ComposeStack {
	t.Helper()

	return composeUp(ctx, t, ConvertTestName(t.Name()), composeYAML, "")
}

// ComposeUpWithName deploys a compose stack from a YAML string with a custom stack name.
func ComposeUpWithName(ctx context.Context, t *testing.T, stackName, composeYAML string) *ComposeStack {
	t.Helper()

	return composeUp(ctx, t, stackName, composeYAML, "")
}

// ComposeUpFile deploys a compose stack from a compose file path.
func ComposeUpFile(ctx context.Context, t *testing.T, stackName, composeFilePath string) *ComposeStack {
	t.Helper()

	return composeUp(ctx, t, stackName, "", composeFilePath)
}

func composeUp(ctx context.Context, t *testing.T, stackName, composeYAML, composeFilePath string) *ComposeStack {
	t.Helper()

	dockerCli, err := NewDockerCli()
	if err != nil {
		t.Fatalf("failed to create docker cli: %v", err)
	}

	svc, err := compose.NewComposeService(dockerCli)
	if err != nil {
		t.Fatalf("failed to create compose service: %v", err)
	}

	filePath := composeFilePath

	if composeYAML != "" {
		tmpFile, err := os.CreateTemp(t.TempDir(), "compose-*.yaml")
		if err != nil {
			t.Fatalf("failed to create temp compose file: %v", err)
		}

		if _, err = tmpFile.WriteString(composeYAML); err != nil {
			t.Fatalf("failed to write compose file: %v", err)
		}

		_ = tmpFile.Close()
		filePath = tmpFile.Name()
	}

	options, err := composeCli.NewProjectOptions(
		[]string{filePath},
		composeCli.WithName(stackName),
	)
	if err != nil {
		t.Fatalf("failed to create project options: %v", err)
	}

	project, err := options.LoadProject(ctx)
	if err != nil {
		t.Fatalf("failed to load compose project: %v", err)
	}

	const maxRetries = 3

	for i := range maxRetries {
		err = svc.Up(ctx, project, api.UpOptions{
			Create: api.CreateOptions{RemoveOrphans: true},
			Start:  api.StartOptions{Wait: true},
		})
		if err == nil {
			break
		}

		if i < maxRetries-1 {
			t.Logf("failed to start stack (attempt %d/%d): %v, retrying...", i+1, maxRetries, err)
		}
	}

	if err != nil {
		t.Fatalf("failed to start compose stack %q: %v", stackName, err)
	}

	dockerClient, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("failed to create docker client: %v", err)
	}

	stack := &ComposeStack{
		Name:      stackName,
		Service:   svc,
		DockerCli: dockerCli,
		Client:    dockerClient,
	}

	t.Cleanup(func() {
		if err := svc.Down(context.WithoutCancel(ctx), stackName, api.DownOptions{
			RemoveOrphans: true,
			Volumes:       true,
			Images:        "all",
		}); err != nil {
			t.Errorf("failed to stop compose stack %q: %v", stackName, err)
		}

		dockerClient.Close()
	})

	return stack
}

// ServiceContainerID returns the container ID for a service in the stack.
func (s *ComposeStack) ServiceContainerID(ctx context.Context, t *testing.T, serviceName string) string {
	t.Helper()

	containers, err := s.Service.Ps(ctx, s.Name, api.PsOptions{All: true})
	if err != nil {
		t.Fatalf("failed to list containers for stack %q: %v", s.Name, err)
	}

	for _, c := range containers {
		if c.Service == serviceName {
			return c.ID
		}
	}

	t.Fatalf("no container found for service %q in stack %q", serviceName, s.Name)

	return ""
}

// MappedPort returns the host port mapped to a container's port for a given service.
// containerPort can be "80", "80/tcp", or "80/udp". If no protocol is specified, "tcp" is assumed.
func (s *ComposeStack) MappedPort(ctx context.Context, t *testing.T, serviceName, containerPort string) string {
	t.Helper()

	if !strings.Contains(containerPort, "/") {
		containerPort += "/tcp"
	}

	containerID := s.ServiceContainerID(ctx, t, serviceName)

	result, err := s.Client.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("failed to inspect container %s: %v", containerID, err)
	}

	portKey, err := network.ParsePort(containerPort)
	if err != nil {
		t.Fatalf("failed to parse port %q: %v", containerPort, err)
	}

	bindings := result.Container.NetworkSettings.Ports[portKey]
	if len(bindings) == 0 {
		t.Fatalf("no port binding found for %s on service %q", containerPort, serviceName)
	}

	return bindings[0].HostPort
}

// Endpoint returns host:port for a service's container port.
// containerPort can be "80", "80/tcp", or "80/udp". If no protocol is specified, "tcp" is assumed.
func (s *ComposeStack) Endpoint(ctx context.Context, t *testing.T, serviceName, containerPort string) string {
	t.Helper()

	return "localhost:" + s.MappedPort(ctx, t, serviceName, containerPort)
}

// Exec runs a command inside a container of the given service and returns exit code + output reader.
// The output reader contains multiplexed stdout/stderr from the Docker API.
func (s *ComposeStack) Exec(ctx context.Context, t *testing.T, serviceName string, cmd []string) (int, io.Reader) {
	t.Helper()

	containerID := s.ServiceContainerID(ctx, t, serviceName)

	execResp, err := s.Client.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		t.Fatalf("failed to create exec in container %s: %v", containerID, err)
	}

	attachResp, err := s.Client.ExecAttach(ctx, execResp.ID, client.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("failed to attach to exec in container %s: %v", containerID, err)
	}

	output, err := io.ReadAll(attachResp.Reader)
	attachResp.Close()

	if err != nil {
		t.Fatalf("failed to read exec output: %v", err)
	}

	inspectResp, err := s.Client.ExecInspect(ctx, execResp.ID, client.ExecInspectOptions{})
	if err != nil {
		t.Fatalf("failed to inspect exec: %v", err)
	}

	return inspectResp.ExitCode, strings.NewReader(string(output))
}
