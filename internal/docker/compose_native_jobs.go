package docker

import (
	"context"
	"log/slog"
	"maps"
	"slices"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
)

// composeServiceWithoutNativeJobs wraps api.Compose and disables native Compose jobs for Create, Up and Start.
//
// Since Compose v5.8.0, Create, Up and Start register jobs with the Docker Engine jobs extension.
// They also fail when the Engine cannot run a scheduled job.
// doco-cd runs jobs on its own scheduler, so the Engine must not register them.
// All other methods pass through unchanged.
type composeServiceWithoutNativeJobs struct {
	api.Compose
}

// newComposeService creates a Compose service that never hands native jobs to the Docker Engine.
// Use it instead of compose.NewComposeService.
func newComposeService(dockerCli command.Cli) (api.Compose, error) {
	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return nil, err
	}

	return composeServiceWithoutNativeJobs{Compose: service}, nil
}

// withoutNativeJobs returns a copy of the project in which all native Compose jobs are disabled.
// It returns the same project if the project has no jobs.
//
// Compose rejects and registers only the jobs in Project.Jobs, so the copy moves all jobs to DisabledJobs.
// Compose finds orphan jobs through Project.AllJobs, which includes DisabledJobs.
// Thus the default remove_orphans does not delete Engine jobs that the compose file still declares.
// The original project keeps its jobs, so doco-cd can still read them.
func withoutNativeJobs(project *types.Project) *types.Project {
	if project == nil || (len(project.Jobs) == 0 && len(project.DisabledJobs) == 0) {
		return project
	}

	stripped := *project
	stripped.DisabledJobs = project.AllJobs()
	stripped.Jobs = nil

	return &stripped
}

// warnSkippedNativeJobs warns that doco-cd does not schedule or register the native jobs of the project.
// Native jobs are silently skipped otherwise, so users would expect runs that never happen.
func warnSkippedNativeJobs(logger *slog.Logger, project *types.Project) {
	if project == nil || logger == nil {
		return
	}

	jobs := project.AllJobs()
	if len(jobs) == 0 {
		return
	}

	logger.Warn("native Compose jobs are not run by doco-cd and are not registered with the Docker Engine, "+
		"use the doco-cd job labels on services to schedule jobs",
		slog.Any("jobs", slices.Sorted(maps.Keys(jobs))))
}

// warnSwarmNativeJobs warns that Swarm mode ignores native Compose jobs.
// The Swarm deployer converts services only. Swarm job modes have no schedule trigger,
// and the Docker Engine jobs extension runs node-local containers, not Swarm services.
func warnSwarmNativeJobs(logger *slog.Logger, project *types.Project) {
	if project == nil || logger == nil {
		return
	}

	jobs := project.AllJobs()
	if len(jobs) == 0 {
		return
	}

	logger.Warn("native Compose jobs are not supported in Swarm mode and are ignored",
		slog.Any("jobs", slices.Sorted(maps.Keys(jobs))))
}

// Create creates the containers of the project without registering native jobs.
func (c composeServiceWithoutNativeJobs) Create(ctx context.Context, project *types.Project, options api.CreateOptions) error {
	return c.Compose.Create(ctx, withoutNativeJobs(project), options)
}

// Up creates and starts the project without registering native jobs.
func (c composeServiceWithoutNativeJobs) Up(ctx context.Context, project *types.Project, options api.UpOptions) error {
	return c.Compose.Up(ctx, withoutNativeJobs(project), options)
}

// Start starts the containers of the project without checking native jobs against the Docker Engine.
func (c composeServiceWithoutNativeJobs) Start(ctx context.Context, projectName string, options api.StartOptions) error {
	options.Project = withoutNativeJobs(options.Project)

	return c.Compose.Start(ctx, projectName, options)
}
