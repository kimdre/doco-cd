package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/source/oci"
)

const (
	apiPath     = "/v1/api"
	webhookPath = "/v1/webhook"
	healthPath  = "/v1/health"
)

// registerApiEndpoints registers the API endpoints based on the application configuration and
// returns a list of all enabled endpoints.
func registerApiEndpoints(c *app.Config, h *handlerData, log *logger.Logger, mux *http.ServeMux) []string {
	var enabledEndpoints []string

	type endpoint struct {
		path    string
		handler http.HandlerFunc
	}

	// Register health endpoint
	enabledEndpoints = append(enabledEndpoints, healthPath)
	mux.HandleFunc(healthPath, h.HealthCheckHandler)
	log.Debug("register health endpoint", slog.String("path", healthPath))

	// Register API handlers based on configuration
	if c.ApiSecret != "" {
		enabledEndpoints = append(enabledEndpoints, apiPath)

		endpoints := []endpoint{
			{apiPath + "/runs", h.GetDeploymentRunsHandler},
			{apiPath + "/run/{jobID}", h.GetDeploymentRunHandler},
			{apiPath + "/jobs", h.GetScheduledJobsHandler},
			{apiPath + "/job/{jobName}/run", h.TriggerScheduledJobHandler},
			{apiPath + "/projects", h.GetProjectsApiHandler},
			{apiPath + "/project/{projectName}", h.ProjectApiHandler},
			{apiPath + "/project/{projectName}/{action}", h.ProjectActionApiHandler},
			{apiPath + "/stacks", h.GetStacksApiHandler},
			{apiPath + "/stack/{stackName}", h.StackApiHandler},
			{apiPath + "/stack/{stackName}/{action}", h.StackActionApiHandler},
			{apiPath + "/poll/run", h.TriggerPollHandler},
		}

		for _, ep := range endpoints {
			mux.HandleFunc(ep.path, ep.handler)
			log.Debug("register api endpoint", slog.String("path", ep.path))
		}
	} else {
		log.Info("api endpoints disabled, no api secret configured")
	}

	if c.WebhookSecret != "" {
		enabledEndpoints = append(enabledEndpoints, webhookPath)

		endpoints := []endpoint{
			{webhookPath, h.WebhookHandler},
			{webhookPath + "/{customTarget}", h.WebhookHandler},
		}

		for _, ep := range endpoints {
			mux.HandleFunc(ep.path, ep.handler)
			log.Debug("register webhook endpoint", slog.String("path", ep.path))
		}
	} else {
		log.Info("webhook endpoints disabled, no webhook secret configured")
	}

	return enabledEndpoints
}

// GetScheduledJobsHandler handles API requests to list scheduler-managed jobs.
func (h *handlerData) GetScheduledJobsHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.GenID()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !requireMethod(w, jobLog, r, http.MethodGet) {
		return
	}

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	stackName := getQueryParam(r, w, jobLog, jobID, "stack", "string", "").(string)

	jobs, err := scheduler.ListJobs(r.Context(), h.dockerCli, stackName)
	if err != nil {
		errMsg := "failed to list scheduled jobs"
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

		return
	}

	JSONResponse(w, jobs, jobID, http.StatusOK)
}

// GetDeploymentRunsHandler returns recent deployment runs tracked by doco-cd.
func (h *handlerData) GetDeploymentRunsHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.GenID()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	if !requireMethod(w, jobLog, r, http.MethodGet) {
		return
	}

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	limit := 50

	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			JSONError(w, "invalid parameter: limit", "'limit' parameter must be a positive integer", jobID, http.StatusBadRequest)
			return
		}

		limit = min(n, 200)
	}

	status, err := normalizeDeploymentRunStatus(r.URL.Query().Get("status"))
	if err != nil {
		JSONError(w, err.Error(), "valid status values: accepted, running, succeeded, failed, skipped", jobID, http.StatusBadRequest)
		return
	}

	trigger, err := normalizeDeploymentRunTrigger(r.URL.Query().Get("trigger"))
	if err != nil {
		JSONError(w, err.Error(), "valid trigger values: webhook, poll, scheduled_job", jobID, http.StatusBadRequest)
		return
	}

	if h.runTracker == nil {
		JSONResponse(w, []deploymentRun{}, jobID, http.StatusOK)
		return
	}

	runs := h.runTracker.List(limit, trigger, status)
	JSONResponse(w, runs, jobID, http.StatusOK)
}

// GetDeploymentRunHandler returns details for one deployment run identified by jobID.
func (h *handlerData) GetDeploymentRunHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.GenID()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	if !requireMethod(w, jobLog, r, http.MethodGet) {
		return
	}

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	requestedJobID := strings.TrimSpace(r.PathValue("jobID"))
	if requestedJobID == "" {
		JSONError(w, "missing job id", "", jobID, http.StatusBadRequest)
		return
	}

	if h.runTracker == nil {
		JSONError(w, "run not found: "+requestedJobID, "", jobID, http.StatusNotFound)
		return
	}

	run, ok := h.runTracker.Get(requestedJobID)
	if !ok {
		JSONError(w, "run not found: "+requestedJobID, "", jobID, http.StatusNotFound)
		return
	}

	JSONResponse(w, run, jobID, http.StatusOK)
}

// TriggerScheduledJobHandler handles API requests to run one configured scheduled job immediately.
func (h *handlerData) TriggerScheduledJobHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.GenID()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !requireMethod(w, jobLog, r, http.MethodPost) {
		return
	}

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	jobName := r.PathValue("jobName")
	if jobName == "" {
		err := errors.New("missing job name")
		jobLog.Error(err.Error())
		JSONError(w, err, "", jobID, http.StatusBadRequest)

		return
	}

	stackName := getQueryParam(r, w, jobLog, jobID, "stack", "string", "").(string)

	wait := getQueryParam(r, w, jobLog, jobID, "wait", "bool", true).(bool)
	if h.runTracker != nil {
		h.runTracker.TrackAccepted(jobID, deploymentRunTriggerScheduledJob)
		h.runTracker.SetMetadata(jobID, "scheduled:"+jobName, stackName, "")

		if wait {
			h.runTracker.MarkRunning(jobID)
		}
	}

	triggerFn := func(ctx context.Context) error {
		jobLog.Info("scheduled job run triggered via API", slog.String("job", jobName), slog.String("stack", stackName))

		runID, err := scheduler.TriggerNow(ctx, h.dockerCli, h.log.Logger, jobName, stackName, h.secretProvider)

		runLog := jobLog
		if runID != "" {
			runLog = runLog.With(slog.String("scheduled_run_id", runID))
		}

		if err == nil {
			return nil
		}

		runLog.With(logger.ErrAttr(err)).Error("failed to trigger scheduled job run", slog.String("job", jobName), slog.String("stack", stackName))

		return err
	}

	if !wait {
		go func(ctx context.Context) {
			defer func() {
				if r := recover(); r != nil {
					logRecoveredPanic(jobLog, "scheduled job run", r)

					if h.runTracker != nil {
						h.runTracker.MarkFailed(jobID, "scheduled job run panicked")
					}
				}
			}()

			if h.runTracker != nil {
				h.runTracker.MarkRunning(jobID)
			}

			err := triggerFn(ctx)
			if h.runTracker != nil {
				if err != nil {
					h.runTracker.MarkFailed(jobID, err.Error())
				} else {
					h.runTracker.MarkSucceeded(jobID, "scheduled job run completed")
				}
			}
		}(context.WithoutCancel(r.Context()))

		JSONResponse(w, "scheduled job run accepted", jobID, http.StatusAccepted)

		return
	}

	err := triggerFn(r.Context())
	if err != nil {
		if h.runTracker != nil {
			h.runTracker.MarkFailed(jobID, err.Error())
		}

		switch {
		case errors.Is(err, scheduler.ErrScheduledJobNotFound):
			JSONError(w, err.Error(), "", jobID, http.StatusNotFound)
		case errors.Is(err, scheduler.ErrScheduledJobDisabled), errors.Is(err, scheduler.ErrScheduledJobAmbiguous):
			JSONError(w, err.Error(), "", jobID, http.StatusConflict)
		default:
			JSONError(w, "failed to trigger scheduled job run", err.Error(), jobID, http.StatusInternalServerError)
		}

		return
	}

	JSONResponse(w, "scheduled job run completed", jobID, http.StatusOK)

	if h.runTracker != nil {
		h.runTracker.MarkSucceeded(jobID, "scheduled job run completed")
	}
}

// HealthCheckHandler handles health check requests.
func (h *handlerData) HealthCheckHandler(w http.ResponseWriter, _ *http.Request) {
	var (
		err     error
		errType error
	)

	jobID := id.GenID()

	metadata := notification.Metadata{
		JobID:      jobID,
		Repository: "healthcheck",
		Stack:      "",
		Revision:   "",
	}

	err, errType = docker.VerifyDockerAPIAccess()
	if err != nil {
		onError(w, h.log.With(logger.ErrAttr(err)), errType.Error(), err.Error(), http.StatusServiceUnavailable, metadata, err)

		return
	}

	JSONResponse(w, "healthy", jobID, http.StatusOK)
}

// getQueryParam retrieves and validates a query parameter from the HTTP request.
func getQueryParam(r *http.Request, w http.ResponseWriter, log *slog.Logger, jobID, key, keyType string, defaultVal any) any {
	queryParam := r.URL.Query().Get(key)
	if queryParam == "" {
		return defaultVal
	}

	ErrInvalidParam := errors.New("invalid parameter")

	switch keyType {
	case "bool":
		value, err := strconv.ParseBool(queryParam)
		if err != nil {
			err = fmt.Errorf("%w: %s", ErrInvalidParam, key)
			errMsg := "'" + key + "' parameter must be true or false"
			log.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, err, errMsg, jobID, http.StatusBadRequest)

			return defaultVal
		}

		return value
	case "int":
		value, err := strconv.Atoi(queryParam)
		if err != nil {
			err = fmt.Errorf("%w: %s", ErrInvalidParam, key)
			errMsg := "'" + key + "' parameter must be a integer"
			log.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, err, errMsg, jobID, http.StatusBadRequest)

			return defaultVal
		}

		return value
	case "string":
		return queryParam
	default:
		err := errors.New("invalid key type")
		errMsg := "key type must be 'bool', 'int' or 'string'"
		log.With(logger.ErrAttr(err)).Error(errMsg)
		JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

		return defaultVal
	}
}

// requireMethod checks if the HTTP request method matches the required method and sends an error response if it does not.
func requireMethod(w http.ResponseWriter, log *slog.Logger, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}

	err := ErrInvalidHTTPMethod
	log.Error(err.Error())
	JSONError(w, err.Error(), "requires method: "+method, "", http.StatusMethodNotAllowed)

	return false
}

// ProjectApiHandler handles API requests to get or delete a Docker Compose project.
func (h *handlerData) ProjectApiHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Add a job id to the context to track deployments in the logs
	jobID := id.GenID()
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

	switch r.Method {
	case http.MethodGet:
		containers, err := docker.GetProjectContainers(ctx, h.dockerCli, projectName)
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
		timeoutSec := getQueryParam(r, w, jobLog, jobID, "timeout", "int", 30).(int)
		timeout := time.Duration(timeoutSec) * time.Second
		removeVolumes := getQueryParam(r, w, jobLog, jobID, "volumes", "bool", true).(bool)
		removeImages := getQueryParam(r, w, jobLog, jobID, "images", "bool", true).(bool)

		jobLog.Info("removing project", slog.String("project", projectName), slog.Bool("remove_volumes", removeVolumes), slog.Bool("remove_images", removeImages))

		err := docker.RemoveProject(ctx, h.dockerCli, projectName, timeout, removeVolumes, removeImages)
		if err != nil {
			errMsg := "failed to remove project: " + projectName
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

			return
		}

		JSONResponse(w, "project removed: "+projectName, jobID, http.StatusOK)
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
	jobID := id.GenID()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	showAll := getQueryParam(r, w, jobLog, jobID, "all", "bool", false).(bool)

	projects, err := docker.GetProjects(ctx, h.dockerCli, showAll)
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

	var err error

	// Add a job id to the context to track deployments in the logs
	jobID := id.GenID()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	projectName := r.PathValue("projectName")
	if projectName == "" {
		err = errors.New("missing project name")
		jobLog.Error(err.Error())
		JSONError(w, err, "", jobID, http.StatusBadRequest)

		return
	}

	timeoutSec := getQueryParam(r, w, jobLog, jobID, "timeout", "int", 30).(int)
	timeout := time.Duration(timeoutSec) * time.Second

	containers, err := docker.GetProjectContainers(ctx, h.dockerCli, projectName)
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

	action := r.PathValue("action")
	switch action {
	case "start":
		if !requireMethod(w, jobLog, r, http.MethodPost) {
			return
		}

		jobLog.Info("starting project", slog.String("project", projectName))

		err := docker.StartProject(ctx, h.dockerCli, projectName, timeout)
		if err != nil {
			errMsg := "failed to start project"
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

			return
		}

		JSONResponse(w, "project started: "+projectName, jobID, http.StatusOK)
	case "stop":
		if !requireMethod(w, jobLog, r, http.MethodPost) {
			return
		}

		jobLog.Info("stopping project", slog.String("project", projectName))

		err := docker.StopProject(ctx, h.dockerCli, projectName, timeout)
		if err != nil {
			errMsg := "failed to stop project"
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

			return
		}

		JSONResponse(w, "project stopped: "+projectName, jobID, http.StatusOK)
	case "restart":
		if !requireMethod(w, jobLog, r, http.MethodPost) {
			return
		}

		jobLog.Info("restarting project", slog.String("project", projectName))

		err := docker.RestartProject(ctx, h.dockerCli, projectName, timeout)
		if err != nil {
			errMsg := "failed to restart project"
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

			return
		}

		JSONResponse(w, "project restarted: "+projectName, jobID, http.StatusOK)
	default:
		jobLog.Error(restAPI.ErrInvalidAction.Error())
		JSONError(w, restAPI.ErrInvalidAction.Error(), "action not supported: "+action, jobID, http.StatusBadRequest)

		return
	}
}

// StackActionApiHandler handles API requests to manage Docker Swarm stacks.
func (h *handlerData) StackActionApiHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var err error

	// Add a job id to the context to track deployments in the logs
	jobID := id.GenID()
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

	serviceName := getQueryParam(r, w, jobLog, jobID, "service", "string", "").(string)
	waitForServices := getQueryParam(r, w, jobLog, jobID, "wait", "bool", true).(bool)

	services, err := swarm.GetStackServices(ctx, h.dockerCli.Client(), stackName)
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

	action := r.PathValue("action")
	switch action {
	case "scale":
		if !requireMethod(w, jobLog, r, http.MethodPost) {
			return
		}

		replicas := getQueryParam(r, w, jobLog, jobID, "replicas", "int", -1).(int)
		if replicas < 0 {
			err = errors.New("missing or invalid replicas parameter")
			errMsg := "'replicas' parameter is required and must be a non-negative integer"
			jobLog.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, err, errMsg, jobID, http.StatusBadRequest)

			return
		}

		for _, svc := range services {
			svcName := svc.Spec.Name
			if serviceName != "" {
				if svcName != fmt.Sprintf("%s_%s", stackName, serviceName) {
					continue
				}
			}

			jobLog.Info("scaling service", slog.String("service", svcName), slog.Int("replicas", replicas))

			err = swarm.ScaleService(ctx, h.dockerCli, svcName, uint64(replicas), waitForServices, false)
			if err != nil {
				if errors.Is(err, swarm.ErrNotReplicatedService) {
					jobLog.Debug("skipping non-replicated service for scale action", slog.String("service", svcName))
					continue
				}

				errMsg := "failed to scale service"
				jobLog.With(logger.ErrAttr(err)).Error(errMsg)
				JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

				return
			}

			if serviceName != "" {
				JSONResponse(w, fmt.Sprintf("service scaled: %s to %d replicas", serviceName, replicas), jobID, http.StatusOK)
				return
			}
		}

		JSONResponse(w, fmt.Sprintf("stack scaled: %s to %d replicas", stackName, replicas), jobID, http.StatusOK)
	case "restart":
		if !requireMethod(w, jobLog, r, http.MethodPost) {
			return
		}

		for _, svc := range services {
			svcName := svc.Spec.Name
			if serviceName != "" {
				if svcName != fmt.Sprintf("%s_%s", stackName, serviceName) {
					continue
				}
			}

			// Job services cannot be updated with UpdateConfig present; treat restart as a no-op.
			if svc.Spec.Mode.ReplicatedJob != nil || svc.Spec.Mode.GlobalJob != nil {
				jobLog.Debug("skipping restart for job-mode service", slog.String("service", svcName))
				continue
			}

			jobLog.Info("restarting service", slog.String("service", svcName))

			// Swarm restart supports replicated/global and skips job-mode services.
			err = docker.RestartService(ctx, h.dockerCli.Client(), svcName)
			if err != nil {
				if errors.Is(err, docker.ErrJobServiceRestartNotSupported) {
					jobLog.Debug("skipping restart for job-mode service", slog.String("service", svcName))
					continue
				}

				errMsg := "failed to restart service"
				jobLog.With(logger.ErrAttr(err)).Error(errMsg)
				JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

				return
			}

			if serviceName != "" {
				JSONResponse(w, "service restarted: "+svcName, jobID, http.StatusOK)
				return
			}
		}

		JSONResponse(w, "stack restarted: "+stackName, jobID, http.StatusOK)
	case "run":
		if !requireMethod(w, jobLog, r, http.MethodPost) {
			return
		}

		var reRunCounter int64

		for _, svc := range services {
			svcName := svc.Spec.Name
			if serviceName != "" && svcName != fmt.Sprintf("%s_%s", stackName, serviceName) {
				continue
			}

			jobLog.Info("retriggering job service", slog.String("service", svcName))

			err = docker.RerunJobService(ctx, h.dockerCli.Client(), svcName)
			if err != nil {
				if errors.Is(err, docker.ErrNotAJobService) {
					jobLog.Debug("skipping non-job service for run action", slog.String("service", svcName))
					continue
				}

				errMsg := "failed to retrigger job service"
				jobLog.With(logger.ErrAttr(err)).Error(errMsg)
				JSONError(w, err, errMsg, jobID, http.StatusInternalServerError)

				return
			}

			reRunCounter++

			if serviceName != "" {
				JSONResponse(w, "job retriggered: "+svcName, jobID, http.StatusOK)
				return
			}
		}

		if reRunCounter == 0 {
			JSONError(w, "no job services found to retrigger in stack: "+stackName, "", jobID, http.StatusNotFound)
			return
		}

		JSONResponse(w, strconv.FormatInt(reRunCounter, 10)+" job(s) retriggered in stack: "+stackName, jobID, http.StatusOK)

	default:
		jobLog.Error(restAPI.ErrInvalidAction.Error())
		JSONError(w, restAPI.ErrInvalidAction.Error(), "action not supported: "+action, jobID, http.StatusBadRequest)

		return
	}
}

// StackApiHandler handles API requests to get or delete a Docker Swarm stack.
func (h *handlerData) StackApiHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Add a job id to the context to track deployments in the logs
	jobID := id.GenID()
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

	switch r.Method {
	case http.MethodGet:
		services, err := swarm.GetStackServices(ctx, h.dockerCli.Client(), stackName)
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
		jobLog.Info("removing stack", slog.String("stack", stackName))

		err := docker.RemoveSwarmStack(ctx, h.dockerCli, stackName)
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
	jobID := id.GenID()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	stacks, err := swarm.GetStacks(ctx, h.dockerCli.Client())
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

// TriggerPollHandler handles API requests to trigger a poll of the configured repositories.
// This can be used to manually trigger a poll outside the planned intervals,
// for example after a failed deployment or to check for new commits after a network outage.
func (h *handlerData) TriggerPollHandler(w http.ResponseWriter, r *http.Request) {
	var err error

	// Add a job id to the context to track deployments in the logs
	jobID := id.GenID()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !requireMethod(w, jobLog, r, http.MethodPost) {
		return
	}

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	wait := getQueryParam(r, w, jobLog, jobID, "wait", "bool", true).(bool)
	if h.runTracker != nil {
		h.runTracker.TrackAccepted(jobID, deploymentRunTriggerPoll)

		if wait {
			h.runTracker.MarkRunning(jobID)
		}
	}

	decoder := json.NewDecoder(r.Body)
	defer func() {
		_ = r.Body.Close()
	}()

	var pollConfigs []poll.Config
	if err := decoder.Decode(&pollConfigs); err != nil {
		errMsg := "failed to decode json in body"
		h.log.Error(errMsg, logger.ErrAttr(err))
		JSONError(w, errMsg, err.Error(), jobID, http.StatusBadRequest)

		if h.runTracker != nil {
			h.runTracker.MarkFailed(jobID, errMsg+": "+err.Error())
		}

		return
	}

	// Set default values for api-called poll jobs
	for i, p := range pollConfigs {
		p.RunOnce = true
		p.Interval = 0

		err = p.Validate()
		if err != nil {
			errMsg := fmt.Sprintf("invalid poll configuration at index %d", i)
			h.log.Error(errMsg, logger.ErrAttr(err))
			JSONError(w, errMsg, err.Error(), jobID, http.StatusBadRequest)

			if h.runTracker != nil {
				h.runTracker.MarkFailed(jobID, errMsg+": "+err.Error())
			}

			return
		}

		pollConfigs[i] = p
	}

	if len(pollConfigs) > 0 {
		h.log.Info("poll triggered via API")

		var wg sync.WaitGroup

		errs := make(chan error, len(pollConfigs))

		pollCtx := r.Context()
		if !wait {
			pollCtx = context.WithoutCancel(pollCtx)
		}

		runner := h.runPoll
		if runner == nil {
			runner = RunPoll
		}

		if h.runTracker != nil {
			repository := "multiple"
			if len(pollConfigs) == 1 {
				repository = pollRepositoryName(pollConfigs[0])
			}

			h.runTracker.SetMetadata(jobID, repository, "", "")
		}

		for _, p := range pollConfigs {
			wg.Add(1)

			go func(ctx context.Context, pollConfig poll.Config) {
				defer wg.Done()
				defer func() {
					if recovered := recover(); recovered != nil {
						logRecoveredPanic(jobLog, "poll run", recovered)

						errs <- errors.New("poll run panicked")
					}
				}()

				repository := pollRepositoryName(pollConfig)
				metadata := notification.Metadata{
					Repository: repository,
					Stack:      "",
					Revision:   notification.GetRevision(pollConfig.Reference, ""),
					JobID:      jobID,
				}

				errs <- runner(ctx, pollConfig, h.appConfig, h.dataMountPoint, h.dockerCli, h.log.Logger, metadata, h.secretProvider, pollTriggerDefault)
			}(pollCtx, p)
		}

		completeTracking := func() {
			wg.Wait()
			close(errs)

			var failedRuns int

			for runErr := range errs {
				if runErr != nil {
					failedRuns++
				}
			}

			if h.runTracker != nil {
				if failedRuns > 0 {
					h.runTracker.MarkFailed(jobID, fmt.Sprintf("%d/%d poll jobs failed", failedRuns, len(pollConfigs)))
				} else {
					h.runTracker.MarkSucceeded(jobID, "poll jobs complete")
				}
			}
		}

		if wait {
			completeTracking()
			JSONResponse(w, "poll jobs complete", jobID, http.StatusOK)

			return
		}

		if h.runTracker != nil {
			h.runTracker.MarkRunning(jobID)
		}

		JSONResponse(w, "poll jobs started", jobID, http.StatusAccepted)

		go completeTracking()

		return
	}

	err = errors.New("no poll configuration provided in request body")
	jobLog.Error(err.Error())
	JSONError(w, err.Error(), "", jobID, http.StatusBadRequest)

	if h.runTracker != nil {
		h.runTracker.MarkFailed(jobID, err.Error())
	}
}

func pollRepositoryName(cfg poll.Config) string {
	sourceType := config.NormalizeSourceType(cfg.Source)
	if sourceType == config.SourceTypeOCI {
		return oci.RepositoryNameFromArtifact(cfg.SourceUrl)
	}

	return git.GetRepoName(cfg.SourceUrl)
}
