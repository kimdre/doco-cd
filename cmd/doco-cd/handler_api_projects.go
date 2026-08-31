package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/common/id"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
)

// ProjectApiHandler handles API requests to get or delete a Docker Compose project.
func (h *handlerData) ProjectApiHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Add a job id to the context to track deployments in the logs
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	projectName := r.PathValue("projectName")
	if projectName == "" {
		err := errors.New("missing project name")
		jobLog.Error(err.Error())
		JSONError(w, err, "", jobID, http.StatusBadRequest)

		return
	}

	dockerCli, contextName, ok := h.dockerCliForRequest(w, r, jobLog, jobID)
	if !ok {
		return
	}

	jobLog = jobLog.With(slog.String("context", contextName))

	switch r.Method {
	case http.MethodGet:
		containers, err := docker.GetProjectContainers(ctx, dockerCli, projectName)
		if err != nil {
			errMsg := "failed to get project: " + projectName
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

			return
		}

		if len(containers) == 0 {
			JSONError(w, "project not found: "+projectName, "", jobID, http.StatusNotFound)
			return
		}

		JSONResponse(w, containers, jobID, http.StatusOK)
	case http.MethodDelete:
		timeoutSec, ok := getIntQueryParam(r, w, jobLog, jobID, "timeout", defaultProjectActionTimeout)
		if !ok {
			return
		}

		removeVolumes, ok := getBoolQueryParam(r, w, jobLog, jobID, "volumes", true)
		if !ok {
			return
		}

		removeImages, ok := getBoolQueryParam(r, w, jobLog, jobID, "images", true)
		if !ok {
			return
		}

		result, err := h.destroyProject(ctx, dockerCli, projectName, timeoutSec, removeVolumes, removeImages, jobLog)
		if err != nil {
			if errors.Is(err, errInvalidProjectTimeout) {
				JSONError(w, err.Error(), "", jobID, http.StatusBadRequest)

				return
			}

			errMsg := "failed to remove project: " + projectName
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

			return
		}

		JSONResponse(w, result.Message, jobID, http.StatusOK)
	default:
		err := ErrInvalidHTTPMethod
		h.log.Error(err.Error())
		JSONError(w, err.Error(), "", "", http.StatusMethodNotAllowed)

		return
	}
}

// GetProjectsApiHandler handles API requests to list Docker Compose projects.
func (h *handlerData) GetProjectsApiHandler(w http.ResponseWriter, r *http.Request) {
	var err error

	ctx := r.Context()

	if r.Method != http.MethodGet {
		err = ErrInvalidHTTPMethod
		h.log.Error(err.Error())
		JSONError(w, err.Error(), "requires GET method", "", http.StatusMethodNotAllowed)

		return
	}

	// Add a job id to the context to track deployments in the logs
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	dockerCli, contextName, ok := h.dockerCliForRequest(w, r, jobLog, jobID)
	if !ok {
		return
	}

	jobLog = jobLog.With(slog.String("context", contextName))

	showAll, ok := getBoolQueryParam(r, w, jobLog, jobID, "all", false)
	if !ok {
		return
	}

	projects, err := docker.GetProjects(ctx, dockerCli, showAll)
	if err != nil {
		errMsg := "failed to get projects"
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

		return
	}

	if len(projects) == 0 {
		JSONError(w, "no projects found", "", jobID, http.StatusNotFound)
		return
	}

	JSONResponse(w, projects, jobID, http.StatusOK)
}

// ProjectActionApiHandler handles API requests to manage Docker Compose projects.
func (h *handlerData) ProjectActionApiHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Add a job id to the context to track deployments in the logs
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	projectName := r.PathValue("projectName")
	if projectName == "" {
		err := errors.New("missing project name")
		jobLog.Error(err.Error())
		JSONError(w, err, "", jobID, http.StatusBadRequest)

		return
	}

	dockerCli, contextName, ok := h.dockerCliForRequest(w, r, jobLog, jobID)
	if !ok {
		return
	}

	jobLog = jobLog.With(slog.String("context", contextName))

	timeoutSec, ok := getIntQueryParam(r, w, jobLog, jobID, "timeout", defaultProjectActionTimeout)
	if !ok {
		return
	}

	action := r.PathValue("action")

	operation, err := h.resolveProjectAction(ctx, dockerCli, projectName, action)
	if err != nil {
		if errors.Is(err, errProjectNotFound) {
			JSONError(w, err.Error(), "", jobID, http.StatusNotFound)

			return
		}

		if lookupErr, ok := errors.AsType[*projectLookupError](err); ok {
			errMsg := "failed to get project: " + projectName
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, errMsg, lookupErr.cause.Error(), jobID, http.StatusInternalServerError)

			return
		}

		if errors.Is(err, restAPI.ErrInvalidAction) {
			jobLog.Error(restAPI.ErrInvalidAction.Error())
			JSONError(w, restAPI.ErrInvalidAction.Error(), "action not supported: "+action, jobID, http.StatusBadRequest)

			return
		}

		errMsg := "failed to " + action + " project"
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

		return
	}

	if !requireMethod(w, jobLog, r, http.MethodPost) {
		return
	}

	result, err := h.executeProjectAction(ctx, operation, timeoutSec, jobLog)
	if err != nil {
		if errors.Is(err, errInvalidProjectTimeout) {
			JSONError(w, err.Error(), "", jobID, http.StatusBadRequest)

			return
		}

		errMsg := "failed to " + action + " project"
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

		return
	}

	JSONResponse(w, result.Message, jobID, http.StatusOK)
}

var errProjectNotFound = errors.New("project not found")

type projectLookupError struct {
	projectName string
	cause       error
}

func (e *projectLookupError) Error() string {
	return fmt.Sprintf("failed to get project: %s: %v", e.projectName, e.cause)
}

func (e *projectLookupError) Unwrap() error {
	return e.cause
}

type destroyProjectResult struct {
	ProjectName string
	Message     string
	Volumes     bool
	Images      bool
}

type projectActionResult struct {
	ProjectName string
	Action      string
	Message     string
}

type projectActionOperation struct {
	projectName string
	action      string
	message     string
	execute     func(context.Context, time.Duration, *slog.Logger) error
}

func (h *handlerData) destroyProject(ctx context.Context, dockerCli command.Cli, projectName string, timeoutSec int, removeVolumes, removeImages bool, jobLog *slog.Logger) (destroyProjectResult, error) {
	timeout, err := projectActionTimeout(timeoutSec)
	if err != nil {
		return destroyProjectResult{}, err
	}

	if dockerCli == nil {
		return destroyProjectResult{}, errors.New("docker cli is required")
	}

	jobLog.Info("removing project", slog.String("project", projectName), slog.Bool("remove_volumes", removeVolumes), slog.Bool("remove_images", removeImages))

	if err := docker.RemoveProject(ctx, dockerCli, projectName, timeout, removeVolumes, removeImages); err != nil {
		return destroyProjectResult{}, err
	}

	return destroyProjectResult{
		ProjectName: projectName,
		Message:     "project removed: " + projectName,
		Volumes:     removeVolumes,
		Images:      removeImages,
	}, nil
}

func (h *handlerData) runProjectAction(ctx context.Context, dockerCli command.Cli, projectName, action string, timeoutSec int, jobLog *slog.Logger) (projectActionResult, error) {
	operation, err := h.resolveProjectAction(ctx, dockerCli, projectName, action)
	if err != nil {
		return projectActionResult{}, err
	}

	return h.executeProjectAction(ctx, operation, timeoutSec, jobLog)
}

func (h *handlerData) resolveProjectAction(ctx context.Context, dockerCli command.Cli, projectName, action string) (projectActionOperation, error) {
	if err := h.requireProject(ctx, dockerCli, projectName); err != nil {
		return projectActionOperation{}, err
	}

	operation := projectActionOperation{projectName: projectName, action: action}

	switch action {
	case "start":
		operation.message = "project started: " + projectName
		operation.execute = func(ctx context.Context, timeout time.Duration, jobLog *slog.Logger) error {
			jobLog.Info("starting project", slog.String("project", projectName))

			return docker.StartProject(ctx, dockerCli, projectName, timeout)
		}
	case "stop":
		operation.message = "project stopped: " + projectName
		operation.execute = func(ctx context.Context, timeout time.Duration, jobLog *slog.Logger) error {
			jobLog.Info("stopping project", slog.String("project", projectName))

			return docker.StopProject(ctx, dockerCli, projectName, timeout)
		}
	case "restart":
		operation.message = "project restarted: " + projectName
		operation.execute = func(ctx context.Context, timeout time.Duration, jobLog *slog.Logger) error {
			jobLog.Info("restarting project", slog.String("project", projectName))

			return docker.RestartProject(ctx, dockerCli, projectName, timeout)
		}
	default:
		return projectActionOperation{}, fmt.Errorf("%w: action not supported: %s", restAPI.ErrInvalidAction, action)
	}

	return operation, nil
}

var errInvalidProjectTimeout = errors.New("invalid project timeout")

func projectActionTimeout(timeoutSec int) (time.Duration, error) {
	if timeoutSec < 1 || int64(timeoutSec) > maxProjectActionTimeout {
		return 0, fmt.Errorf("%w: must be between 1 and %d seconds", errInvalidProjectTimeout, maxProjectActionTimeout)
	}

	return time.Duration(timeoutSec) * time.Second, nil
}

func (h *handlerData) requireProject(ctx context.Context, dockerCli command.Cli, projectName string) error {
	if dockerCli == nil {
		return errors.New("docker cli is required")
	}

	containers, err := docker.GetProjectContainers(ctx, dockerCli, projectName)
	if err != nil {
		return &projectLookupError{projectName: projectName, cause: err}
	}

	if len(containers) == 0 {
		return fmt.Errorf("%w: %s", errProjectNotFound, projectName)
	}

	return nil
}

func (h *handlerData) executeProjectAction(ctx context.Context, operation projectActionOperation, timeoutSec int, jobLog *slog.Logger) (projectActionResult, error) {
	timeout, err := projectActionTimeout(timeoutSec)
	if err != nil {
		return projectActionResult{}, err
	}

	if err := operation.execute(ctx, timeout, jobLog); err != nil {
		return projectActionResult{}, err
	}

	return projectActionResult{
		ProjectName: operation.projectName,
		Action:      operation.action,
		Message:     operation.message,
	}, nil
}
