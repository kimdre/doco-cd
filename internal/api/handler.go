package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/common/validation"
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

// Dependencies contains the runtime services required by the REST API handlers.
type Dependencies struct {
	AppConfig             *app.Config             `validate:"required,nostructlevel"`
	Logger                *logger.Logger          `validate:"required,nostructlevel"`
	DockerCLI             command.Cli             `validate:"required,nostructlevel"`
	Contexts              *docker.ContextRegistry `validate:"required,nostructlevel"`
	Runs                  RunOperations           `validate:"required,nostructlevel"`
	HealthFailureReporter HealthFailureReporter   `validate:"required"`
}

// Handler adapts REST requests to Docker and control-plane operations.
type Handler struct {
	appConfig             *app.Config
	log                   *logger.Logger
	dockerCli             command.Cli
	contexts              *docker.ContextRegistry
	controlPlaneRuns      RunOperations
	healthFailureReporter HealthFailureReporter
}

// NewHandler validates dependencies and constructs the REST API adapter.
func NewHandler(dependencies Dependencies) (*Handler, error) {
	if err := validation.Validate(dependencies); err != nil {
		return nil, fmt.Errorf("validate API dependencies: %w", err)
	}

	if dependencies.Logger.Logger == nil {
		return nil, errors.New("api logger is required")
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
