package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/scheduler"
)

// RunOperations is the control-plane surface consumed by the REST API.
type RunOperations interface {
	List(limit int, trigger, status string) []controlplane.Run
	Get(jobID string) (controlplane.Run, bool)
	ListScheduledJobs(ctx context.Context, contextName, stackName string) ([]scheduler.JobInfo, error)
	TriggerScheduledJob(ctx context.Context, jobID, contextName, jobName, stackName string, wait bool) (string, error)
	TriggerPoll(ctx context.Context, configs []poll.Config, wait bool, log *slog.Logger) (string, error)
}

// HealthFailureReporter preserves application-level health failure reporting
// without coupling the API package to the webhook implementation.
type HealthFailureReporter func(
	w http.ResponseWriter,
	log *slog.Logger,
	jobID string,
	failureType error,
	cause error,
)

type Dependencies struct {
	AppConfig             *app.Config
	Logger                *logger.Logger
	DockerCLI             command.Cli
	Contexts              *docker.ContextRegistry
	Runs                  RunOperations
	HealthFailureReporter HealthFailureReporter
}

type Handler struct {
	appConfig             *app.Config
	log                   *logger.Logger
	dockerCli             command.Cli
	contexts              *docker.ContextRegistry
	controlPlaneRuns      RunOperations
	healthFailureReporter HealthFailureReporter
}

func NewHandler(dependencies Dependencies) (*Handler, error) {
	switch {
	case dependencies.AppConfig == nil:
		return nil, errors.New("api application config is required")
	case dependencies.Logger == nil || dependencies.Logger.Logger == nil:
		return nil, errors.New("api logger is required")
	case dependencies.DockerCLI == nil:
		return nil, errors.New("api docker CLI is required")
	case dependencies.Contexts == nil:
		return nil, errors.New("api docker context registry is required")
	case dependencies.Runs == nil:
		return nil, errors.New("api control-plane run operations are required")
	case dependencies.HealthFailureReporter == nil:
		return nil, errors.New("api health failure reporter is required")
	}

	return &Handler{
		appConfig:             dependencies.AppConfig,
		log:                   dependencies.Logger,
		dockerCli:             dependencies.DockerCLI,
		contexts:              dependencies.Contexts,
		controlPlaneRuns:      dependencies.Runs,
		healthFailureReporter: dependencies.HealthFailureReporter,
	}, nil
}
