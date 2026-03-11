package test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	composeCli "github.com/compose-spec/compose-go/v2/cli"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/container"
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

// composeOptions holds the configuration for [ComposeUp].
type composeOptions struct {
	yaml        string
	filePath    string
	name        string
	pruneImages bool
	noWait      bool
	waitTimeout time.Duration // Maximum time to wait for containers to be healthy in [ComposeUp]. Default is 30 seconds.
}

// ComposeOption configures a [ComposeUp] call.
type ComposeOption func(*composeOptions)

// WithYAML sets the compose source to an inline YAML string.
func WithYAML(yaml string) ComposeOption {
	return func(o *composeOptions) {
		o.yaml = yaml
	}
}

// WithFile sets the compose source to a file path.
func WithFile(filePath string) ComposeOption {
	return func(o *composeOptions) {
		o.filePath = filePath
	}
}

// WithNoWait disables waiting for containers to be healthy after starting the stack.
func WithNoWait() ComposeOption {
	return func(o *composeOptions) {
		o.noWait = true
	}
}

// WithWaitTimeout sets the maximum time to wait for containers to be healthy in [ComposeUp]. Default is 30 seconds.
func WithWaitTimeout(timeout time.Duration) ComposeOption {
	return func(o *composeOptions) {
		o.waitTimeout = timeout
	}
}

// WithName sets a custom stack name.
// If not set, the stack name defaults to a sanitized version of the test name.
func WithName(name string) ComposeOption {
	return func(o *composeOptions) {
		o.name = name
	}
}

// WithPruneImages removes all images when the stack is taken down during cleanup.
func WithPruneImages() ComposeOption {
	return func(o *composeOptions) {
		o.pruneImages = true
	}
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

func loadOpts(opts ...ComposeOption) composeOptions {
	var o composeOptions
	for _, opt := range opts {
		opt(&o)
	}

	if o.waitTimeout == 0 {
		o.waitTimeout = 30 * time.Second
	}

	return o
}

// ComposeUp deploys a compose stack and registers automatic cleanup via t.Cleanup.
// Use [WithYAML] or [WithFile] to set the compose source, and [WithName] to set a custom stack name.
func ComposeUp(ctx context.Context, t *testing.T, opts ...ComposeOption) *ComposeStack {
	t.Helper()

	o := loadOpts(opts...)

	stackName := o.name
	if stackName == "" {
		stackName = ConvertTestName(t.Name())
	}

	filePath := o.filePath
	if o.yaml != "" {
		filePath = writeComposeYAML(t, o.yaml)
	}

	dockerCli, err := NewDockerCli()
	if err != nil {
		t.Fatalf("failed to create docker cli: %v", err)
	}

	svc, err := compose.NewComposeService(dockerCli)
	if err != nil {
		t.Fatalf("failed to create compose service: %v", err)
	}

	options, err := composeCli.NewProjectOptions(
		[]string{filePath},
		composeCli.WithWorkingDirectory(filepath.Dir(filePath)),
		composeCli.WithName(stackName),
	)
	if err != nil {
		t.Fatalf("failed to create project options: %v", err)
	}

	project, err := options.LoadProject(ctx)
	if err != nil {
		t.Fatalf("failed to load compose project: %v", err)
	}

	// Set the CustomLabels that compose uses internally to track containers
	// by project/service. Without these, the Start phase cannot find the
	// containers created by Create ("service X has no container to start").
	for i, s := range project.Services {
		s.CustomLabels = map[string]string{
			api.ProjectLabel:     project.Name,
			api.ServiceLabel:     s.Name,
			api.WorkingDirLabel:  project.WorkingDir,
			api.ConfigFilesLabel: strings.Join(project.ComposeFiles, ","),
			api.VersionLabel:     api.ComposeVersion,
			api.OneoffLabel:      "False",
		}
		project.Services[i] = s
	}

	dockerClient, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("failed to create docker client: %v", err)
	}

	t.Cleanup(func() {
		if err := dockerClient.Close(); err != nil {
			t.Errorf("failed to close docker client: %v", err)
		}
	})

	t.Cleanup(func() {
		downOpts := api.DownOptions{
			RemoveOrphans: true,
			Volumes:       true,
		}
		if o.pruneImages {
			downOpts.Images = "all"
		} else {
			downOpts.Images = "local"
		}

		if err := svc.Down(context.WithoutCancel(ctx), stackName, downOpts); err != nil {
			t.Errorf("failed to stop compose stack %q: %v", stackName, err)
		}
	})

	err = retry.New(
		retry.Delay(1*time.Second),
		retry.DelayType(retry.BackOffDelay),
		retry.Attempts(3),
		retry.RetryIf(func(err error) bool {
			// Retry if error contains "No such image"
			return strings.Contains(err.Error(), "No such image:")
		}),
	).Do(
		func() error {
			return svc.Up(ctx, project, api.UpOptions{
				Create: api.CreateOptions{RemoveOrphans: true},
				Start: api.StartOptions{
					Project:     project,
					Wait:        !o.noWait,
					WaitTimeout: o.waitTimeout,
				},
			})
		})
	if err != nil {
		t.Fatalf("failed to start compose stack %q: %v", stackName, err)
	}

	stack := &ComposeStack{
		Name:      stackName,
		Service:   svc,
		DockerCli: dockerCli,
		Client:    dockerClient,
	}

	return stack
}

// writeComposeYAML writes a compose YAML string to a temporary file and returns its path.
func writeComposeYAML(t *testing.T, yaml string) string {
	t.Helper()

	tmpFile, err := os.CreateTemp(t.TempDir(), "compose-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp compose file: %v", err)
	}

	if _, err = tmpFile.WriteString(yaml); err != nil {
		t.Fatalf("failed to write compose file: %v", err)
	}

	_ = tmpFile.Close()

	return tmpFile.Name()
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

// WaitForStack waits until all containers in the stack are running and healthy (if healthcheck is defined).
func WaitForStack(ctx context.Context, t *testing.T, compose api.Compose, projectName string, timeout time.Duration) ([]api.ContainerSummary, error) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for stack %q to be ready", projectName)
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled while waiting for stack %q to be ready: %w", projectName, ctx.Err())
		case <-ticker.C:
			containers, err := compose.Ps(ctx, projectName, api.PsOptions{All: true})
			if err != nil {
				return nil, fmt.Errorf("failed to list containers for stack %q: %w", projectName, err)
			}

			if len(containers) == 0 {
				continue
			}

			ready := true

			for _, c := range containers {
				if c.State != container.StateRunning {
					ready = false
					break
				}

				if c.Health != "" && c.Health != container.Healthy {
					ready = false
					break
				}
			}

			if ready {
				return containers, nil
			}
		}
	}
}
