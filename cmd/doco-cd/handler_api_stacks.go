package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/docker/cli/cli/command"
	dockerswarmtypes "github.com/moby/moby/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/common/id"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
)

type stackActionResult struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
}

var errStackNotFound = errors.New("stack not found")

var errNoApplicableStackServices = errors.New("no applicable services found")

type stackServiceNotFoundError struct {
	Service string
}

func (e *stackServiceNotFoundError) Error() string {
	return "service not found: " + e.Service
}

type stackServiceActionError struct {
	Service string
	Cause   error
}

func (e *stackServiceActionError) Error() string {
	return fmt.Sprintf("stack action failed for service %s: %v", e.Service, e.Cause)
}

func (e *stackServiceActionError) Unwrap() error {
	return e.Cause
}

func (h *handlerData) getStackServices(ctx context.Context, dockerCli command.Cli, stack string) ([]dockerswarmtypes.Service, error) {
	if dockerCli == nil {
		return nil, errors.New("docker cli is required")
	}

	services, err := swarm.GetStackServices(ctx, dockerCli.Client(), stack)
	if err != nil {
		return nil, err
	}

	if len(services) == 0 {
		return nil, fmt.Errorf("%w: %s", errStackNotFound, stack)
	}

	return services, nil
}

func (h *handlerData) runStackAction(ctx context.Context, dockerCli command.Cli, stack, action, service string, replicas int, wait bool, jobLog *slog.Logger) ([]stackActionResult, error) {
	services, err := h.getStackServices(ctx, dockerCli, stack)
	if err != nil {
		return nil, err
	}

	return h.runStackActionOnServices(ctx, dockerCli, services, stack, action, service, replicas, wait, jobLog)
}

func (h *handlerData) runStackActionOnServices(
	ctx context.Context,
	dockerCli command.Cli,
	services []dockerswarmtypes.Service,
	stack, action, service string,
	replicas int,
	wait bool,
	jobLog *slog.Logger,
) ([]stackActionResult, error) {
	if action == "scale" && replicas < 0 {
		return nil, errors.New("'replicas' parameter is required and must be a non-negative integer")
	}

	if action != "scale" && action != "restart" && action != "run" {
		return nil, fmt.Errorf("%w: %s", restAPI.ErrInvalidAction, action)
	}

	results := make([]stackActionResult, 0, len(services))
	matched := false
	succeeded := false

	fullServiceName := ""
	if service != "" {
		fullServiceName = stack + "_" + service
	}

	for _, svc := range services {
		svcName := svc.Spec.Name
		if fullServiceName != "" && svcName != fullServiceName {
			continue
		}

		matched = true

		result := stackActionResult{Service: svcName, Status: "ok"}

		var err error

		switch action {
		case "scale":
			jobLog.Info("scaling service", slog.String("service", svcName), slog.Int("replicas", replicas))

			err = swarm.ScaleService(ctx, dockerCli, svcName, uint64(replicas), wait, false) // #nosec G115 -- replicas is validated as non-negative above.
			if errors.Is(err, swarm.ErrNotReplicatedService) {
				result.Status = "skipped"
				result.Reason = swarm.ErrNotReplicatedService.Error()
			}
		case "restart":
			if svc.Spec.Mode.ReplicatedJob != nil || svc.Spec.Mode.GlobalJob != nil {
				result.Status = "skipped"
				result.Reason = docker.ErrJobServiceRestartNotSupported.Error()
				err = docker.ErrJobServiceRestartNotSupported
			} else {
				jobLog.Info("restarting service", slog.String("service", svcName))

				err = docker.RestartService(ctx, dockerCli.Client(), svcName)
				if errors.Is(err, docker.ErrJobServiceRestartNotSupported) {
					result.Status = "skipped"
					result.Reason = docker.ErrJobServiceRestartNotSupported.Error()
				}
			}
		case "run":
			jobLog.Info("retriggering job service", slog.String("service", svcName))

			err = docker.RerunJobService(ctx, dockerCli.Client(), svcName)
			if errors.Is(err, docker.ErrNotAJobService) {
				result.Status = "skipped"
				result.Reason = docker.ErrNotAJobService.Error()
			}
		}

		if err != nil && result.Status != "skipped" {
			return results, &stackServiceActionError{Service: svcName, Cause: err}
		}

		if result.Status == "skipped" {
			jobLog.Debug("skipping service for stack action", slog.String("service", svcName), slog.String("action", action), slog.String("reason", result.Reason))
		}

		results = append(results, result)
		if result.Status == "ok" {
			succeeded = true
		}
	}

	if !matched {
		return results, &stackServiceNotFoundError{Service: fullServiceName}
	}

	if !succeeded {
		return results, errNoApplicableStackServices
	}

	return results, nil
}

func (h *handlerData) removeStack(ctx context.Context, dockerCli command.Cli, stack string, jobLog *slog.Logger) error {
	if dockerCli == nil {
		return errors.New("docker cli is required")
	}

	jobLog.Info("removing stack", slog.String("stack", stack))

	return docker.RemoveSwarmStack(ctx, dockerCli, stack)
}

// StackActionApiHandler handles API requests to manage Docker Swarm stacks.
func (h *handlerData) StackActionApiHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var err error

	// Add a job id to the context to track deployments in the logs
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	stackName := r.PathValue("stackName")
	if stackName == "" {
		err = errors.New("missing stack name")
		jobLog.Error(err.Error())
		JSONError(w, err, "", jobID, http.StatusBadRequest)

		return
	}

	dockerCli, contextName, ok := h.dockerCliForRequest(w, r, jobLog, jobID)
	if !ok {
		return
	}

	jobLog = jobLog.With(slog.String("context", contextName))

	services, err := h.getStackServices(ctx, dockerCli, stackName)
	if err != nil {
		if errors.Is(err, errStackNotFound) {
			JSONError(w, "stack not found: "+stackName, "", jobID, http.StatusNotFound)

			return
		}

		errMsg := "failed to get stack: " + stackName
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

		return
	}

	action := r.PathValue("action")
	if action != "scale" && action != "restart" && action != "run" {
		jobLog.Error(restAPI.ErrInvalidAction.Error())
		JSONError(w, restAPI.ErrInvalidAction.Error(), "action not supported: "+action, jobID, http.StatusBadRequest)

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
			JSONError(w, err, errMsg, jobID, http.StatusBadRequest)

			return
		}
	}

	results, err := h.runStackActionOnServices(ctx, dockerCli, services, stackName, action, serviceName, replicas, waitForServices, jobLog)
	if err != nil {
		if _, ok := errors.AsType[*stackServiceNotFoundError](err); ok {
			JSONError(w, err.Error(), "", jobID, http.StatusNotFound)

			return
		}

		if errors.Is(err, errNoApplicableStackServices) {
			errMsg := map[string]string{
				"scale":   "no services found to scale in stack: " + stackName,
				"restart": "no services found to restart in stack: " + stackName,
				"run":     "no job services found to retrigger in stack: " + stackName,
			}[action]
			JSONError(w, errMsg, "", jobID, http.StatusNotFound)

			return
		}

		errMsg := map[string]string{
			"scale":   "failed to scale service",
			"restart": "failed to restart service",
			"run":     "failed to retrigger job service",
		}[action]
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

		return
	}

	// runStackActionOnServices returns errNoApplicableStackServices when nothing
	// succeeded, so at least one service succeeded on this path.
	switch action {
	case "scale":
		if serviceName != "" {
			JSONResponse(w, fmt.Sprintf("service scaled: %s to %d replicas", serviceName, replicas), jobID, http.StatusOK)

			return
		}

		JSONResponse(w, fmt.Sprintf("stack scaled: %s to %d replicas", stackName, replicas), jobID, http.StatusOK)
	case "restart":
		if serviceName != "" {
			JSONResponse(w, "service restarted: "+stackName+"_"+serviceName, jobID, http.StatusOK)

			return
		}

		JSONResponse(w, "stack restarted: "+stackName, jobID, http.StatusOK)
	case "run":
		if serviceName != "" {
			JSONResponse(w, "job retriggered: "+stackName+"_"+serviceName, jobID, http.StatusOK)

			return
		}

		successCount := 0

		for _, result := range results {
			if result.Status == "ok" {
				successCount++
			}
		}

		JSONResponse(w, strconv.Itoa(successCount)+" job(s) retriggered in stack: "+stackName, jobID, http.StatusOK)
	}
}

// StackApiHandler handles API requests to get or delete a Docker Swarm stack.
func (h *handlerData) StackApiHandler(w http.ResponseWriter, r *http.Request) {
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

	stackName := r.PathValue("stackName")
	if stackName == "" {
		err := errors.New("missing stack name")
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
		services, err := swarm.GetStackServices(ctx, dockerCli.Client(), stackName)
		if err != nil {
			errMsg := "failed to get stack: " + stackName
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

			return
		}

		if len(services) == 0 {
			JSONError(w, "stack not found: "+stackName, "", jobID, http.StatusNotFound)
			return
		}

		JSONResponse(w, services, jobID, http.StatusOK)
	case http.MethodDelete:
		err := h.removeStack(ctx, dockerCli, stackName, jobLog)
		if err != nil {
			errMsg := "failed to remove stack: " + stackName
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

			return
		}

		JSONResponse(w, "stack removed: "+stackName, jobID, http.StatusOK)
	default:
		err := ErrInvalidHTTPMethod
		h.log.Error(err.Error())
		JSONError(w, err.Error(), "", "", http.StatusMethodNotAllowed)

		return
	}
}

// GetStacksApiHandler handles API requests to list Docker Swarm stacks.
func (h *handlerData) GetStacksApiHandler(w http.ResponseWriter, r *http.Request) {
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

	stacks, err := swarm.GetStacks(ctx, dockerCli.Client())
	if err != nil {
		errMsg := "failed to get stacks"
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

		return
	}

	if len(stacks) == 0 {
		JSONError(w, "no stacks found", "", jobID, http.StatusNotFound)
		return
	}

	JSONResponse(w, stacks, jobID, http.StatusOK)
}
