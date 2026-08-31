package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/kimdre/doco-cd/internal/common/id"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/restapi"
)

// StackActionApiHandler applies an action to matching services in a Swarm stack.
func (h *Handler) StackActionApiHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var err error

	// Add a job id to the context to track deployments in the logs
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !restapi.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restapi.ErrInvalidApiKey.Error())
		restapi.JSONError(w, restapi.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	stackName := r.PathValue("stackName")
	if stackName == "" {
		err = errors.New("missing stack name")
		jobLog.Error(err.Error())
		restapi.JSONError(w, err, "", jobID, http.StatusBadRequest)

		return
	}

	dockerCli, contextName, ok := h.dockerCliForRequest(w, r, jobLog, jobID)
	if !ok {
		return
	}

	jobLog = jobLog.With(slog.String("context", contextName))

	services, err := controlplane.GetStackServices(ctx, dockerCli, stackName)
	if err != nil {
		if errors.Is(err, controlplane.ErrStackNotFound) {
			restapi.JSONError(w, "stack not found: "+stackName, "", jobID, http.StatusNotFound)

			return
		}

		errMsg := "failed to get stack: " + stackName
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		restapi.JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

		return
	}

	action := r.PathValue("action")
	if action != "scale" && action != "restart" && action != "run" {
		jobLog.Error(restapi.ErrInvalidAction.Error())
		restapi.JSONError(w, restapi.ErrInvalidAction.Error(), "action not supported: "+action, jobID, http.StatusBadRequest)

		return
	}

	if !requireMethod(w, jobLog, r, http.MethodPost) {
		return
	}

	serviceName := r.URL.Query().Get("service")

	waitForServices, ok := getBoolQueryParam(r, w, jobLog, jobID, "wait", true)
	if !ok {
		return
	}

	replicas := -1
	if action == "scale" {
		replicas, ok = getIntQueryParam(r, w, jobLog, jobID, "replicas", -1)
		if !ok {
			return
		}

		if replicas < 0 {
			err = errors.New("missing or invalid replicas parameter")
			errMsg := "'replicas' parameter is required and must be a non-negative integer"
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			restapi.JSONError(w, err, errMsg, jobID, http.StatusBadRequest)

			return
		}
	}

	results, err := controlplane.RunStackActionOnServices(ctx, dockerCli, services, stackName, action, serviceName, replicas, waitForServices, jobLog)
	if err != nil {
		if _, ok := errors.AsType[*controlplane.StackServiceNotFoundError](err); ok {
			restapi.JSONError(w, err.Error(), "", jobID, http.StatusNotFound)

			return
		}

		if errors.Is(err, controlplane.ErrNoApplicableStackServices) {
			errMsg := map[string]string{
				"scale":   "no services found to scale in stack: " + stackName,
				"restart": "no services found to restart in stack: " + stackName,
				"run":     "no job services found to retrigger in stack: " + stackName,
			}[action]
			restapi.JSONError(w, errMsg, "", jobID, http.StatusNotFound)

			return
		}

		errMsg := map[string]string{
			"scale":   "failed to scale service",
			"restart": "failed to restart service",
			"run":     "failed to retrigger job service",
		}[action]
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		restapi.JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

		return
	}

	// runStackActionOnServices returns errNoApplicableStackServices when nothing
	// succeeded, so at least one service succeeded on this path.
	switch action {
	case "scale":
		if serviceName != "" {
			restapi.JSONResponse(w, fmt.Sprintf("service scaled: %s to %d replicas", serviceName, replicas), jobID, http.StatusOK)

			return
		}

		restapi.JSONResponse(w, fmt.Sprintf("stack scaled: %s to %d replicas", stackName, replicas), jobID, http.StatusOK)
	case "restart":
		if serviceName != "" {
			restapi.JSONResponse(w, "service restarted: "+stackName+"_"+serviceName, jobID, http.StatusOK)

			return
		}

		restapi.JSONResponse(w, "stack restarted: "+stackName, jobID, http.StatusOK)
	case "run":
		if serviceName != "" {
			restapi.JSONResponse(w, "job retriggered: "+stackName+"_"+serviceName, jobID, http.StatusOK)

			return
		}

		successCount := 0

		for _, result := range results {
			if result.Status == "ok" {
				successCount++
			}
		}

		restapi.JSONResponse(w, strconv.Itoa(successCount)+" job(s) retriggered in stack: "+stackName, jobID, http.StatusOK)
	}
}

// StackApiHandler returns or removes a single Swarm stack.
func (h *Handler) StackApiHandler(w http.ResponseWriter, r *http.Request) {
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

	stackName := r.PathValue("stackName")
	if stackName == "" {
		err := errors.New("missing stack name")
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
		services, err := swarm.GetStackServices(ctx, dockerCli.Client(), stackName)
		if err != nil {
			errMsg := "failed to get stack: " + stackName
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			restapi.JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

			return
		}

		if len(services) == 0 {
			restapi.JSONError(w, "stack not found: "+stackName, "", jobID, http.StatusNotFound)
			return
		}

		restapi.JSONResponse(w, services, jobID, http.StatusOK)
	case http.MethodDelete:
		err := controlplane.RemoveStack(ctx, dockerCli, stackName, jobLog)
		if err != nil {
			errMsg := "failed to remove stack: " + stackName
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			restapi.JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

			return
		}

		restapi.JSONResponse(w, "stack removed: "+stackName, jobID, http.StatusOK)
	default:
		err := restapi.ErrInvalidHTTPMethod
		h.log.Error(err.Error())
		restapi.JSONError(w, err.Error(), "", "", http.StatusMethodNotAllowed)

		return
	}
}

// GetStacksApiHandler lists Swarm stacks in the requested Docker context.
func (h *Handler) GetStacksApiHandler(w http.ResponseWriter, r *http.Request) {
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

	stacks, err := swarm.GetStacks(ctx, dockerCli.Client())
	if err != nil {
		errMsg := "failed to get stacks"
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		restapi.JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

		return
	}

	if len(stacks) == 0 {
		restapi.JSONError(w, "no stacks found", "", jobID, http.StatusNotFound)
		return
	}

	restapi.JSONResponse(w, stacks, jobID, http.StatusOK)
}
