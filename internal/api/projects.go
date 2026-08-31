package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/kimdre/doco-cd/internal/common/id"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/restapi"
)

// ProjectApiHandler handles API requests to get or delete a Docker Compose project.
func (h *Handler) ProjectApiHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Add a job id to the context to track deployments in the logs
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !restapi.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restapi.ErrInvalidApiKey.Error())
		restapi.JSONError(w, restapi.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	projectName := r.PathValue("projectName")
	if projectName == "" {
		err := errors.New("missing project name")
		jobLog.Error(err.Error())
		restapi.JSONError(w, err, "", jobID, http.StatusBadRequest)

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
			restapi.JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

			return
		}

		if len(containers) == 0 {
			restapi.JSONError(w, "project not found: "+projectName, "", jobID, http.StatusNotFound)
			return
		}

		restapi.JSONResponse(w, containers, jobID, http.StatusOK)
	case http.MethodDelete:
		timeoutSec, ok := getIntQueryParam(r, w, jobLog, jobID, "timeout", controlplane.DefaultProjectActionTimeout)
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

		result, err := controlplane.DestroyProject(ctx, dockerCli, projectName, timeoutSec, removeVolumes, removeImages, jobLog)
		if err != nil {
			if errors.Is(err, controlplane.ErrInvalidProjectTimeout) {
				restapi.JSONError(w, err.Error(), "", jobID, http.StatusBadRequest)

				return
			}

			errMsg := "failed to remove project: " + projectName
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			restapi.JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

			return
		}

		restapi.JSONResponse(w, result.Message, jobID, http.StatusOK)
	default:
		err := restapi.ErrInvalidHTTPMethod
		h.log.Error(err.Error())
		restapi.JSONError(w, err.Error(), "", "", http.StatusMethodNotAllowed)

		return
	}
}

// GetProjectsApiHandler handles API requests to list Docker Compose projects.
func (h *Handler) GetProjectsApiHandler(w http.ResponseWriter, r *http.Request) {
	var err error

	ctx := r.Context()

	if r.Method != http.MethodGet {
		err = restapi.ErrInvalidHTTPMethod
		h.log.Error(err.Error())
		restapi.JSONError(w, err.Error(), "requires GET method", "", http.StatusMethodNotAllowed)

		return
	}

	// Add a job id to the context to track deployments in the logs
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !restapi.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restapi.ErrInvalidApiKey.Error())
		restapi.JSONError(w, restapi.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

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
		restapi.JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

		return
	}

	if len(projects) == 0 {
		restapi.JSONError(w, "no projects found", "", jobID, http.StatusNotFound)
		return
	}

	restapi.JSONResponse(w, projects, jobID, http.StatusOK)
}

// ProjectActionApiHandler handles API requests to manage Docker Compose projects.
func (h *Handler) ProjectActionApiHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Add a job id to the context to track deployments in the logs
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !restapi.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restapi.ErrInvalidApiKey.Error())
		restapi.JSONError(w, restapi.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	projectName := r.PathValue("projectName")
	if projectName == "" {
		err := errors.New("missing project name")
		jobLog.Error(err.Error())
		restapi.JSONError(w, err, "", jobID, http.StatusBadRequest)

		return
	}

	dockerCli, contextName, ok := h.dockerCliForRequest(w, r, jobLog, jobID)
	if !ok {
		return
	}

	jobLog = jobLog.With(slog.String("context", contextName))

	timeoutSec, ok := getIntQueryParam(r, w, jobLog, jobID, "timeout", controlplane.DefaultProjectActionTimeout)
	if !ok {
		return
	}

	action := r.PathValue("action")

	operation, err := controlplane.ResolveProjectAction(ctx, dockerCli, projectName, action)
	if err != nil {
		if errors.Is(err, controlplane.ErrProjectNotFound) {
			restapi.JSONError(w, err.Error(), "", jobID, http.StatusNotFound)

			return
		}

		if lookupErr, ok := errors.AsType[*controlplane.ProjectLookupError](err); ok {
			errMsg := "failed to get project: " + projectName
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			restapi.JSONError(w, errMsg, errors.Unwrap(lookupErr).Error(), jobID, http.StatusInternalServerError)

			return
		}

		if errors.Is(err, restapi.ErrInvalidAction) {
			jobLog.Error(restapi.ErrInvalidAction.Error())
			restapi.JSONError(w, restapi.ErrInvalidAction.Error(), "action not supported: "+action, jobID, http.StatusBadRequest)

			return
		}

		errMsg := "failed to " + action + " project"
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		restapi.JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

		return
	}

	if !requireMethod(w, jobLog, r, http.MethodPost) {
		return
	}

	result, err := controlplane.ExecuteProjectAction(ctx, operation, timeoutSec, jobLog)
	if err != nil {
		if errors.Is(err, controlplane.ErrInvalidProjectTimeout) {
			restapi.JSONError(w, err.Error(), "", jobID, http.StatusBadRequest)

			return
		}

		errMsg := "failed to " + action + " project"
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		restapi.JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

		return
	}

	restapi.JSONResponse(w, result.Message, jobID, http.StatusOK)
}
