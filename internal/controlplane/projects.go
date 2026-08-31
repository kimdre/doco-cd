package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/restapi"
)

const (
	DefaultProjectActionTimeout = 30
	MaxProjectActionTimeout     = math.MaxInt64 / int64(time.Second)
)

var (
	ErrProjectNotFound       = errors.New("project not found")
	ErrInvalidProjectTimeout = errors.New("invalid project timeout")
)

type ProjectLookupError struct {
	projectName string
	cause       error
}

func (e *ProjectLookupError) Error() string {
	return fmt.Sprintf("failed to get project: %s: %v", e.projectName, e.cause)
}

func (e *ProjectLookupError) Unwrap() error {
	return e.cause
}

type DestroyProjectResult struct {
	ProjectName string
	Message     string
	Volumes     bool
	Images      bool
}

type ProjectActionResult struct {
	ProjectName string
	Action      string
	Message     string
}

type ProjectAction struct {
	projectName string
	action      string
	message     string
	execute     func(context.Context, time.Duration, *slog.Logger) error
}

func DestroyProject(
	ctx context.Context,
	dockerCLI command.Cli,
	projectName string,
	timeoutSeconds int,
	removeVolumes bool,
	removeImages bool,
	log *slog.Logger,
) (DestroyProjectResult, error) {
	timeout, err := projectActionTimeout(timeoutSeconds)
	if err != nil {
		return DestroyProjectResult{}, err
	}

	if dockerCLI == nil {
		return DestroyProjectResult{}, errors.New("docker cli is required")
	}

	log.Info("removing project", slog.String("project", projectName), slog.Bool("remove_volumes", removeVolumes), slog.Bool("remove_images", removeImages))

	if err := docker.RemoveProject(ctx, dockerCLI, projectName, timeout, removeVolumes, removeImages); err != nil {
		return DestroyProjectResult{}, err
	}

	return DestroyProjectResult{
		ProjectName: projectName,
		Message:     "project removed: " + projectName,
		Volumes:     removeVolumes,
		Images:      removeImages,
	}, nil
}

func RunProjectAction(
	ctx context.Context,
	dockerCLI command.Cli,
	projectName string,
	action string,
	timeoutSeconds int,
	log *slog.Logger,
) (ProjectActionResult, error) {
	operation, err := ResolveProjectAction(ctx, dockerCLI, projectName, action)
	if err != nil {
		return ProjectActionResult{}, err
	}

	return ExecuteProjectAction(ctx, operation, timeoutSeconds, log)
}

func ResolveProjectAction(ctx context.Context, dockerCLI command.Cli, projectName, action string) (ProjectAction, error) {
	if err := requireProject(ctx, dockerCLI, projectName); err != nil {
		return ProjectAction{}, err
	}

	operation := ProjectAction{projectName: projectName, action: action}

	switch action {
	case "start":
		operation.message = "project started: " + projectName
		operation.execute = func(ctx context.Context, timeout time.Duration, log *slog.Logger) error {
			log.Info("starting project", slog.String("project", projectName))

			return docker.StartProject(ctx, dockerCLI, projectName, timeout)
		}
	case "stop":
		operation.message = "project stopped: " + projectName
		operation.execute = func(ctx context.Context, timeout time.Duration, log *slog.Logger) error {
			log.Info("stopping project", slog.String("project", projectName))

			return docker.StopProject(ctx, dockerCLI, projectName, timeout)
		}
	case "restart":
		operation.message = "project restarted: " + projectName
		operation.execute = func(ctx context.Context, timeout time.Duration, log *slog.Logger) error {
			log.Info("restarting project", slog.String("project", projectName))

			return docker.RestartProject(ctx, dockerCLI, projectName, timeout)
		}
	default:
		return ProjectAction{}, fmt.Errorf("%w: action not supported: %s", restapi.ErrInvalidAction, action)
	}

	return operation, nil
}

func ExecuteProjectAction(ctx context.Context, operation ProjectAction, timeoutSeconds int, log *slog.Logger) (ProjectActionResult, error) {
	timeout, err := projectActionTimeout(timeoutSeconds)
	if err != nil {
		return ProjectActionResult{}, err
	}

	if err := operation.execute(ctx, timeout, log); err != nil {
		return ProjectActionResult{}, err
	}

	return ProjectActionResult{
		ProjectName: operation.projectName,
		Action:      operation.action,
		Message:     operation.message,
	}, nil
}

func projectActionTimeout(timeoutSeconds int) (time.Duration, error) {
	if timeoutSeconds < 1 || int64(timeoutSeconds) > MaxProjectActionTimeout {
		return 0, fmt.Errorf("%w: must be between 1 and %d seconds", ErrInvalidProjectTimeout, MaxProjectActionTimeout)
	}

	return time.Duration(timeoutSeconds) * time.Second, nil
}

func requireProject(ctx context.Context, dockerCLI command.Cli, projectName string) error {
	if dockerCLI == nil {
		return errors.New("docker cli is required")
	}

	containers, err := docker.GetProjectContainers(ctx, dockerCLI, projectName)
	if err != nil {
		return &ProjectLookupError{projectName: projectName, cause: err}
	}

	if len(containers) == 0 {
		return fmt.Errorf("%w: %s", ErrProjectNotFound, projectName)
	}

	return nil
}
