package api

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

const validPollSourceURL = "https://github.com/kimdre/doco-cd_tests.git"

var swarmModeEnabled bool

func TestMain(m *testing.M) {
	dockerCli, err := docker.CreateDockerCli(false)
	if err != nil {
		log.Fatalf("Failed to create docker client: %v", err)
	}

	if err := docker.VerifySocketConnection(); err != nil {
		log.Fatalf("Failed to verify docker socket connection: %v", err)
	}

	swarmModeEnabled, err = swarm.ResolveModeEnabled(context.Background(), dockerCli.Client())
	if err != nil {
		log.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	code := m.Run()

	if err := dockerCli.Client().Close(); err != nil {
		log.Printf("Failed to close Docker client: %v", err)
	}

	os.Exit(code)
}

func TestNewHandlerValidatesDependencies(t *testing.T) {
	dockerCli, err := docker.CreateDockerCli(true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	contexts := docker.NewContextRegistry(dockerCli, docker.ContextRegistryOptions{Quiet: true, SwarmFeatures: true})

	t.Cleanup(func() { _ = contexts.Close() })

	dependencies := Dependencies{
		AppConfig: &app.Config{},
		Logger:    logger.New(logger.LevelCritical),
		DockerCLI: dockerCli,
		Contexts:  contexts,
		Runs:      newTestControlPlaneRuns(t, testControlPlaneRunsOptions{}),
		HealthFailureReporter: func(
			http.ResponseWriter,
			*slog.Logger,
			string,
			error,
			error,
		) {
		},
	}

	if _, err := NewHandler(dependencies); err != nil {
		t.Fatalf("valid dependencies returned an error: %v", err)
	}

	for _, testCase := range []struct {
		name   string
		mutate func(*Dependencies)
	}{
		{name: "application config", mutate: func(d *Dependencies) { d.AppConfig = nil }},
		{name: "logger", mutate: func(d *Dependencies) { d.Logger = nil }},
		{name: "Docker CLI", mutate: func(d *Dependencies) { d.DockerCLI = nil }},
		{name: "context registry", mutate: func(d *Dependencies) { d.Contexts = nil }},
		{name: "run operations", mutate: func(d *Dependencies) { d.Runs = nil }},
		{name: "health failure reporter", mutate: func(d *Dependencies) { d.HealthFailureReporter = nil }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			invalid := dependencies
			testCase.mutate(&invalid)

			if _, err := NewHandler(invalid); err == nil {
				t.Fatal("expected dependency validation error")
			}
		})
	}
}

type testScheduledJobOperations struct {
	listJobs   func(context.Context, string, string) ([]scheduler.JobInfo, error)
	triggerNow func(context.Context, string, string, string, secretprovider.SecretProvider) (string, error)
}

func (f testScheduledJobOperations) ListJobs(ctx context.Context, contextName, stackName string) ([]scheduler.JobInfo, error) {
	if f.listJobs == nil {
		return []scheduler.JobInfo{}, nil
	}

	return f.listJobs(ctx, contextName, stackName)
}

func (f testScheduledJobOperations) TriggerNow(
	ctx context.Context,
	contextName string,
	jobName string,
	stackName string,
	secretProvider secretprovider.SecretProvider,
) (string, error) {
	if f.triggerNow == nil {
		return "", nil
	}

	return f.triggerNow(ctx, contextName, jobName, stackName, secretProvider)
}

type testControlPlaneRunsOptions struct {
	applicationCtx    context.Context
	log               *logger.Logger
	scheduledJobs     controlplane.ScheduledJobOperations
	appConfig         *app.Config
	dataMountPoint    container.MountPoint
	dockerCli         command.Cli
	contexts          *docker.ContextRegistry
	secretProvider    secretprovider.SecretProvider
	pollRunner        controlplane.PollRunner
	maxRunsPerTrigger map[controlplane.RunTrigger]int
	closed            bool
}

func newTestControlPlaneRuns(t testing.TB, options testControlPlaneRunsOptions) *controlplane.Runs {
	t.Helper()

	if options.applicationCtx == nil {
		options.applicationCtx = t.Context()
	}

	if options.log == nil {
		options.log = logger.New(logger.LevelCritical)
	}

	if options.appConfig == nil {
		options.appConfig = &app.Config{}
	}

	if options.scheduledJobs == nil {
		options.scheduledJobs = testScheduledJobOperations{}
	}

	if options.pollRunner == nil {
		options.pollRunner = func(
			context.Context,
			poll.Config,
			*app.Config,
			container.MountPoint,
			command.Cli,
			*docker.ContextRegistry,
			*slog.Logger,
			notification.Metadata,
			secretprovider.SecretProvider,
			string,
		) error {
			return nil
		}
	}

	runs := controlplane.NewRuns(options.applicationCtx, options.log.Logger, controlplane.Dependencies{
		MaxRunsPerTrigger: options.maxRunsPerTrigger,
		ScheduledJobs:     options.scheduledJobs,
		SecretProvider:    options.secretProvider,
		Poll: controlplane.PollDependencies{
			AppConfig:      options.appConfig,
			DataMountPoint: options.dataMountPoint,
			DockerCLI:      options.dockerCli,
			Contexts:       options.contexts,
			Runner:         options.pollRunner,
		},
	})
	t.Cleanup(runs.CloseAndWait)

	if options.closed {
		runs.CloseAndWait()
	}

	return runs
}

func waitForDeploymentRunStatus(t *testing.T, runs *controlplane.Runs, jobID string, want controlplane.RunStatus) controlplane.Run {
	t.Helper()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	for {
		run, ok := runs.Get(jobID)
		if ok && run.Status == want {
			return run
		}

		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("timed out waiting for run %q status %q; last run: %#v", jobID, want, run)
		}
	}
}
