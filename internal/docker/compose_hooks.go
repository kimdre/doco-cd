package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

// ErrComposeHookFailed reports that a Compose lifecycle hook did not complete successfully.
var ErrComposeHookFailed = errors.New("compose lifecycle hook failed")

// ComposeHookPhase identifies a Compose lifecycle hook phase.
// pre_start hooks run in separate runner containers. post_start and pre_stop hooks run in the service container.
type ComposeHookPhase string

const (
	ComposeHookPreStart  ComposeHookPhase = "pre_start"
	ComposeHookPostStart ComposeHookPhase = "post_start"
	ComposeHookPreStop   ComposeHookPhase = "pre_stop"
)

// composeHookPhases is the order of the phases in the lifecycle hooks label.
var composeHookPhases = []ComposeHookPhase{ComposeHookPreStart, ComposeHookPostStart, ComposeHookPreStop}

const (
	composeHookOutputTailBytes = 2048
	composeHookExitPollDelay   = 50 * time.Millisecond
)

// ComposeHookLoader loads deployed Compose service definitions for lifecycle hooks.
// Container labels do not contain hook definitions, so doco-cd reloads the deployed project.
type ComposeHookLoader struct {
	SecretProvider secretprovider.SecretProvider
	Options        ScheduledComposeOptions
	Logger         *slog.Logger

	// loadProject replaces the project loader in tests.
	loadProject func(ctx context.Context, dockerCli command.Cli, labels map[string]string) (*types.Project, error)
}

// LoadServiceProject loads the deployed project of the container that the labels describe.
// The returned project contains only the service of that container.
func (l *ComposeHookLoader) LoadServiceProject(ctx context.Context, dockerCli command.Cli, labels map[string]string) (*types.Project, error) {
	var (
		project *types.Project
		err     error
	)

	if l.loadProject != nil {
		project, err = l.loadProject(ctx, dockerCli, labels)
	} else {
		var ref composeScheduledServiceRef

		ref, err = composeScheduledServiceRefFromLabels(labels)
		if err != nil {
			return nil, err
		}

		project, err = loadComposeScheduledProject(ctx, dockerCli, ref, l.SecretProvider, l.Options)
	}

	if err != nil {
		return nil, err
	}

	serviceName := strings.TrimSpace(labels[api.ServiceLabel])
	if _, ok := project.Services[serviceName]; !ok {
		return nil, fmt.Errorf("%w: service %q is not in the deployed project %q",
			ErrComposeScheduledMetadataUnavailable, serviceName, project.Name)
	}

	return project, nil
}

// HookService returns the deployed service definition of the container that the labels describe.
// If the definition cannot be loaded, it logs the reason and returns false.
// The caller then continues without lifecycle hooks, as before.
func (l *ComposeHookLoader) HookService(ctx context.Context, dockerCli command.Cli, labels map[string]string) (types.ServiceConfig, bool) {
	project, ok := l.hookProject(ctx, dockerCli, labels)
	if !ok {
		return types.ServiceConfig{}, false
	}

	return project.Services[strings.TrimSpace(labels[api.ServiceLabel])], true
}

// hookProject loads the deployed project of the container that the labels describe.
// If the project cannot be loaded, it logs the reason and returns false.
// The caller then continues without lifecycle hooks, as before.
func (l *ComposeHookLoader) hookProject(ctx context.Context, dockerCli command.Cli, labels map[string]string) (*types.Project, bool) {
	if l == nil {
		return nil, false
	}

	project, err := l.LoadServiceProject(ctx, dockerCli, labels)
	if err != nil {
		log := l.Logger
		if log == nil {
			log = slog.Default()
		}

		// Containers without doco-cd deployment metadata cannot have their definition reloaded.
		level := slog.LevelWarn
		if errors.Is(err, ErrComposeScheduledMetadataUnavailable) {
			level = slog.LevelDebug
		}

		log.Log(ctx, level, "cannot load the deployed Compose service definition, continuing without lifecycle hooks",
			slog.String("project", labels[api.ProjectLabel]),
			slog.String("service", labels[api.ServiceLabel]),
			logger.ErrAttr(err),
		)

		return nil, false
	}

	return project, true
}

// composeHookServices loads each service definition at most once during one lifecycle operation.
type composeHookServices struct {
	loader    *ComposeHookLoader
	dockerCli command.Cli
	projects  map[string]*types.Project
}

// newComposeHookServices creates a composeHookServices that caches the loaded service definitions.
func newComposeHookServices(loader *ComposeHookLoader, dockerCli command.Cli) *composeHookServices {
	return &composeHookServices{loader: loader, dockerCli: dockerCli, projects: make(map[string]*types.Project)}
}

// project returns the deployed project of a service. The project contains only this service.
// It does not load the project if the container labels show that the service has no hooks of the phases.
func (c *composeHookServices) project(ctx context.Context, serviceName string, labels map[string]string, phases ...ComposeHookPhase) (*types.Project, bool) {
	if c.loader == nil || !MayHaveComposeHooks(labels, phases...) {
		return nil, false
	}

	if project, ok := c.projects[serviceName]; ok {
		return project, project != nil
	}

	// A failed load is cached as nil, so it is logged once per service.
	project, _ := c.loader.hookProject(ctx, c.dockerCli, labels)
	c.projects[serviceName] = project

	return project, project != nil
}

// service returns the deployed service definition of a service.
// It does not load the project if the container labels show that the service has no hooks of the phases.
func (c *composeHookServices) service(ctx context.Context, serviceName string, labels map[string]string, phases ...ComposeHookPhase) (types.ServiceConfig, bool) {
	project, ok := c.project(ctx, serviceName, labels, phases...)
	if !ok {
		return types.ServiceConfig{}, false
	}

	return project.Services[serviceName], true
}

// HasComposeHooks reports whether the service declares hooks for the phase.
func HasComposeHooks(service types.ServiceConfig, phase ComposeHookPhase) bool {
	if phase == ComposeHookPreStart {
		return len(service.PreStart) > 0
	}

	return len(composeServiceHooks(service, phase)) > 0
}

// composeHookLabelValue returns the value of the lifecycle hooks label: the hook phases of the service, comma-separated.
// The value is empty if the service has no hooks.
func composeHookLabelValue(service types.ServiceConfig) string {
	phases := make([]string, 0, len(composeHookPhases))

	for _, phase := range composeHookPhases {
		if HasComposeHooks(service, phase) {
			phases = append(phases, string(phase))
		}
	}

	return strings.Join(phases, ",")
}

// MayHaveComposeHooks reports whether the service of a container can have hooks of one of the phases.
// Loading the deployed project to find the hooks is costly, because it resolves external secrets.
// The lifecycle hooks label lets doco-cd skip the load for services without hooks.
// Containers that older doco-cd versions created do not have the label. Their hooks are known only after the load.
//
// Compose recreates a container when its hooks change, so the label always matches the deployed hooks.
func MayHaveComposeHooks(labels map[string]string, phases ...ComposeHookPhase) bool {
	value, ok := labels[DocoCDLabels.Deployment.LifecycleHooks]
	if !ok {
		return true
	}

	for declared := range strings.SplitSeq(value, ",") {
		if slices.Contains(phases, ComposeHookPhase(strings.TrimSpace(declared))) {
			return true
		}
	}

	return false
}

// composeServiceHooks returns the hooks of a service for the phase.
func composeServiceHooks(service types.ServiceConfig, phase ComposeHookPhase) []types.ServiceHook {
	switch phase {
	case ComposeHookPreStop:
		return service.PreStop
	case ComposeHookPostStart:
		return service.PostStart
	default:
		return nil
	}
}

// IsComposeHookContainerUnavailable reports whether a hook failed because its container is not running or no longer exists.
// Compose skips the stop of such a container, because the container is already stopped.
func IsComposeHookContainerUnavailable(err error) bool {
	return errdefs.IsConflict(err) || errdefs.IsNotFound(err)
}

// containerIsRunning reports whether the container runs. It returns false if the inspect fails.
func containerIsRunning(ctx context.Context, apiClient client.APIClient, containerID string) bool {
	inspectResult, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return false
	}

	return inspectResult.Container.State != nil && inspectResult.Container.State.Running
}

// RunComposeHooks runs the pre_stop or post_start hooks of a service in a running container.
// doco-cd calls it when it stops, starts, or restarts a container through the Docker API.
// Like Compose, each hook runs as an exec process, and the first failed hook stops the sequence.
func RunComposeHooks(ctx context.Context, apiClient client.APIClient, containerID string, service types.ServiceConfig, phase ComposeHookPhase) error {
	for i, hook := range composeServiceHooks(service, phase) {
		if err := runComposeHook(ctx, apiClient, containerID, hook); err != nil {
			return fmt.Errorf("%w: service %q %s[%d]: %w", ErrComposeHookFailed, service.Name, phase, i, err)
		}
	}

	return nil
}

// runComposeHook runs a single lifecycle hook in a running container.
// It returns the exit code and the last bytes of the output if the hook fails.
func runComposeHook(ctx context.Context, apiClient client.APIClient, containerID string, hook types.ServiceHook) error {
	created, err := apiClient.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		User:         hook.User,
		Privileged:   hook.Privileged,
		Env:          compose.ToMobyEnv(hook.Environment),
		WorkingDir:   hook.WorkingDir,
		Cmd:          hook.Command,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("create exec: %w", err)
	}

	attach, err := apiClient.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("attach exec: %w", err)
	}
	defer attach.Close()

	stdout := &composeHookOutputTail{max: composeHookOutputTailBytes}
	stderr := &composeHookOutputTail{max: composeHookOutputTailBytes}

	copyDone := make(chan error, 1)

	go func() {
		_, copyErr := stdcopy.StdCopy(stdout, stderr, attach.Reader)
		copyDone <- copyErr
	}()

	select {
	case err = <-copyDone:
		if err != nil {
			return fmt.Errorf("read exec output: %w", err)
		}
	case <-ctx.Done():
		// Closing the connection unblocks the reader. The exec process keeps running in the container.
		attach.Close()

		<-copyDone //nolint:errcheck // Closing the connection can cause a read error.

		return ctx.Err()
	}

	exitCode, err := waitComposeHookExit(ctx, apiClient, created.ID)
	if err != nil {
		return err
	}

	if exitCode == 0 {
		return nil
	}

	output := stderr.String()
	if output == "" {
		output = stdout.String()
	}

	if output == "" {
		return fmt.Errorf("exited with status %d", exitCode)
	}

	return fmt.Errorf("exited with status %d: %s", exitCode, output)
}

// waitComposeHookExit reads the exit code of a hook exec process.
// The output stream can close shortly before the engine marks the process as stopped.
func waitComposeHookExit(ctx context.Context, apiClient client.APIClient, execID string) (int, error) {
	for {
		inspect, err := apiClient.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return 0, fmt.Errorf("inspect exec: %w", err)
		}

		if !inspect.Running {
			return inspect.ExitCode, nil
		}

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(composeHookExitPollDelay):
		}
	}
}

// composeHookOutputTail keeps the last bytes of hook output for error messages.
type composeHookOutputTail struct {
	buf []byte
	max int
}

// Write appends p to the tail buffer, keeping only the last max bytes.
func (t *composeHookOutputTail) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)

		return n, nil
	}

	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}

	return n, nil
}

// String returns the last max bytes of the output as a string.
func (t *composeHookOutputTail) String() string {
	return strings.TrimSpace(strings.ToValidUTF8(string(t.buf), ""))
}
