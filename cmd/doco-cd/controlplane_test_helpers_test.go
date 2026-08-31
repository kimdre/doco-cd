package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

type testScheduledJobOperations struct {
	listJobs   func(context.Context, string, string) ([]scheduler.JobInfo, error)
	triggerNow func(context.Context, string, string, string, *secretprovider.SecretProvider) (string, error)
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
	secretProvider *secretprovider.SecretProvider,
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
	secretProvider    *secretprovider.SecretProvider
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
			*secretprovider.SecretProvider,
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
