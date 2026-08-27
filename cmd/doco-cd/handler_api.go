package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/docker/cli/cli/command"
	dockerswarmtypes "github.com/moby/moby/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

const (
	apiPath             = "/v1/api"
	webhookPath         = "/v1/webhook"
	healthPath          = "/v1/health"
	dockerContextHeader = "X-Doco-CD-Context"
)

func (h *handlerData) dockerCliForRequest(
	w http.ResponseWriter,
	r *http.Request,
	jobLog *slog.Logger,
	jobID string,
) (command.Cli, string, bool) {
	values, present := r.URL.Query()["context"]
	if len(values) > 1 {
		JSONError(w, "invalid parameter: context", "'context' parameter must be specified at most once", jobID, http.StatusBadRequest)
		return nil, "", false
	}

	contextName := ""
	if present && len(values) == 1 {
		contextName = docker.NormalizeContextName(values[0])
	}

	displayName := docker.DisplayContextName(contextName)
	w.Header().Set(dockerContextHeader, displayName)

	if h.contexts == nil {
		if contextName != "" {
			JSONError(w, "unknown docker context: "+displayName, "", jobID, http.StatusBadRequest)
			return nil, "", false
		}

		return h.dockerCli, displayName, true
	}

	contextClient, err := h.contexts.Get(r.Context(), contextName)
	if err != nil {
		status := http.StatusInternalServerError
		errMsg := "failed to resolve docker context: " + displayName

		if errors.Is(err, docker.ErrDockerContextNotFound) {
			status = http.StatusBadRequest
			errMsg = "unknown docker context: " + displayName
		}

		jobLog.Error(errMsg, logger.ErrAttr(err))
		JSONError(w, errMsg, err.Error(), jobID, status)

		return nil, "", false
	}

	return contextClient.Cli, contextClient.DisplayName(), true
}

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

	if c.McpEnabled && c.ApiSecret != "" {
		enabledEndpoints = append(enabledEndpoints, mcpPath)
		mux.Handle("POST "+mcpPath, h.newMCPHandler(c))
		log.Debug("register MCP endpoint", slog.String("path", mcpPath))
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
	jobID := id.New()
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

	dockerCli, contextName, ok := h.dockerCliForRequest(w, r, jobLog, jobID)
	if !ok {
		return
	}

	jobLog = jobLog.With(slog.String("context", contextName))
	stackName := r.URL.Query().Get("stack")

	var (
		jobs []scheduler.JobInfo
		err  error
	)
	if h.scheduler != nil {
		jobs, err = h.scheduler.ListJobs(r.Context(), contextName, stackName)
	} else {
		jobs, err = scheduler.ListJobs(r.Context(), dockerCli, stackName)
	}

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
	jobID := id.New()
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

	runs := h.runTracker.List(limit, trigger, status)
	JSONResponse(w, runs, jobID, http.StatusOK)
}

// GetDeploymentRunHandler returns details for one deployment run identified by jobID.
func (h *handlerData) GetDeploymentRunHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.New()
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

	run, ok := h.runTracker.Get(requestedJobID)
	if !ok {
		JSONError(w, "run not found: "+requestedJobID, "", jobID, http.StatusNotFound)
		return
	}

	JSONResponse(w, run, jobID, http.StatusOK)
}

// TriggerScheduledJobHandler handles API requests to run one configured scheduled job immediately.
func (h *handlerData) TriggerScheduledJobHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.New()
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

	dockerCli, contextName, ok := h.dockerCliForRequest(w, r, jobLog, jobID)
	if !ok {
		return
	}

	jobLog = jobLog.With(slog.String("context", contextName))
	stackName := r.URL.Query().Get("stack")

	wait, ok := getBoolQueryParam(r, w, jobLog, jobID, "wait", true)
	if !ok {
		return
	}

	_, err := h.triggerScheduledJobRun(r.Context(), jobID, dockerCli, contextName, jobName, stackName, wait)
	if err != nil {
		switch {
		case isLifecycleCancellation(err):
			JSONError(w, err.Error(), "", jobID, http.StatusServiceUnavailable)
		case errors.Is(err, scheduler.ErrScheduledJobNotFound):
			JSONError(w, err.Error(), "", jobID, http.StatusNotFound)
		case errors.Is(err, scheduler.ErrScheduledJobDisabled), errors.Is(err, scheduler.ErrScheduledJobAmbiguous):
			JSONError(w, err.Error(), "", jobID, http.StatusConflict)
		default:
			JSONError(w, "failed to trigger scheduled job run", err.Error(), jobID, http.StatusInternalServerError)
		}

		return
	}

	if !wait {
		JSONResponse(w, "scheduled job trigger accepted", jobID, http.StatusAccepted)

		return
	}

	JSONResponse(w, "scheduled job triggered", jobID, http.StatusOK)
}

type scheduledJobTrigger func(context.Context, command.Cli, *slog.Logger, string, string, *secretprovider.SecretProvider) (string, error)

var errScheduledJobRunPanicked = errors.New("scheduled job run panicked")

func (h *handlerData) triggerScheduledJobRun(ctx context.Context, jobID string, dockerCli command.Cli, contextName, jobName, stackName string, wait bool) (string, error) {
	if jobID == "" {
		jobID = id.New()
	}

	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("context", contextName))
	h.runTracker.TrackAccepted(jobID, deploymentRunTriggerScheduledJob)
	h.runTracker.SetMetadata(jobID, "scheduled:"+jobName, stackName, "")
	h.runTracker.AddDeployment(jobID, stackName, contextName)

	run := func(ctx context.Context) (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logRecoveredPanic(jobLog, "scheduled job run", recovered)
				h.runTracker.MarkFailed(jobID, errScheduledJobRunPanicked.Error())

				err = errScheduledJobRunPanicked
			}
		}()

		h.runTracker.MarkRunning(jobID)

		jobLog.Info("scheduled job run triggered", slog.String("job", jobName), slog.String("stack", stackName))

		var scheduledRunID string

		switch {
		case h.triggerScheduledJob != nil:
			scheduledRunID, err = h.triggerScheduledJob(ctx, dockerCli, h.log.Logger, jobName, stackName, h.secretProvider)
		case h.scheduler != nil:
			scheduledRunID, err = h.scheduler.TriggerNow(ctx, contextName, jobName, stackName, h.secretProvider)
		default:
			scheduledRunID, err = scheduler.TriggerNow(ctx, dockerCli, h.log.Logger, jobName, stackName, h.secretProvider)
		}

		runLog := jobLog
		if scheduledRunID != "" {
			runLog = runLog.With(slog.String("scheduled_run_id", scheduledRunID))
		}

		if err != nil {
			runLog.With(logger.ErrAttr(err)).Error("failed to trigger scheduled job run", slog.String("job", jobName), slog.String("stack", stackName))
			h.runTracker.MarkFailed(jobID, err.Error())

			return err
		}

		h.runTracker.MarkSucceeded(jobID, "scheduled job trigger completed")

		return nil
	}

	var err error
	if wait {
		err = h.runSynchronous(ctx, run)
	} else {
		err = h.runBackground(ctx, func(ctx context.Context) {
			_ = run(ctx)
		})
	}

	if errors.Is(err, errBackgroundWorkClosed) {
		h.runTracker.MarkFailed(jobID, err.Error())
	}

	return jobID, err
}

func (h *handlerData) runBackground(requestCtx context.Context, run func(context.Context)) error {
	switch {
	case h.backgroundCtx != nil && h.backgroundWork != nil:
		return h.backgroundWork.Go(func() {
			run(h.backgroundCtx)
		})
	case h.backgroundCtx != nil && h.backgroundWG != nil:
		graceful.SafeGo(h.backgroundWG, h.log.Logger, func() {
			run(h.backgroundCtx)
		})
	default:
		run(context.WithoutCancel(requestCtx))
	}

	return nil
}

func (h *handlerData) runSynchronous(requestCtx context.Context, run func(context.Context) error) error {
	if h.backgroundCtx == nil || h.backgroundWork == nil {
		return run(requestCtx)
	}

	release, err := h.backgroundWork.Register()
	if err != nil {
		return err
	}
	defer release()

	runCtx, cancel := context.WithCancel(requestCtx)
	defer cancel()

	stopApplicationCancel := context.AfterFunc(h.backgroundCtx, cancel) //nolint:contextcheck // The synchronous call is intentionally cancelled by either the request or application lifecycle.
	defer stopApplicationCancel()

	return run(runCtx)
}

func isLifecycleCancellation(err error) bool {
	return errors.Is(err, errBackgroundWorkClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// HealthCheckHandler handles health check requests.
func (h *handlerData) HealthCheckHandler(w http.ResponseWriter, _ *http.Request) {
	var (
		err     error
		errType error
	)

	jobID := id.New()

	metadata := notification.Metadata{
		JobID:      jobID,
		Repository: "healthcheck",
		Stack:      "",
		Revision:   "",
	}

	err, errType = docker.VerifyDockerAPIAccess() //nolint:contextcheck // REST health checks must not propagate caller cancellation to notifications.
	if err != nil {
		onError(w, h.log.With(logger.ErrAttr(err)), errType.Error(), err.Error(), http.StatusServiceUnavailable, metadata, err)

		return
	}

	JSONResponse(w, "healthy", jobID, http.StatusOK)
}

// getIntQueryParam parses an optional integer query parameter.
// On invalid input it writes a 400 response and returns false so the handler stops.
func getIntQueryParam(r *http.Request, w http.ResponseWriter, log *slog.Logger, jobID, key string, defaultValue int) (int, bool) {
	queryParam := r.URL.Query().Get(key)
	if queryParam == "" {
		return defaultValue, true
	}

	value, err := strconv.Atoi(queryParam)
	if err != nil {
		err = errors.New("invalid parameter: " + key)
		errMsg := "'" + key + "' parameter must be an integer"
		log.With(logger.ErrAttr(err)).Error(errMsg)
		JSONError(w, err, errMsg, jobID, http.StatusBadRequest)

		return 0, false
	}

	return value, true
}

// getBoolQueryParam parses an optional boolean query parameter.
// On invalid input it writes a 400 response and returns false so the handler stops.
func getBoolQueryParam(r *http.Request, w http.ResponseWriter, log *slog.Logger, jobID, key string, defaultValue bool) (bool, bool) {
	queryParam := r.URL.Query().Get(key)
	if queryParam == "" {
		return defaultValue, true
	}

	value, err := strconv.ParseBool(queryParam)
	if err != nil {
		err = errors.New("invalid parameter: " + key)
		errMsg := "'" + key + "' parameter must be true or false"
		log.With(logger.ErrAttr(err)).Error(errMsg)
		JSONError(w, err, errMsg, jobID, http.StatusBadRequest)

		return false, false
	}

	return value, true
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

		var lookupErr *projectLookupError
		if errors.As(err, &lookupErr) {
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
		var serviceNotFound *stackServiceNotFoundError
		if errors.As(err, &serviceNotFound) {
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

// TriggerPollHandler handles API requests to trigger a poll of the configured repositories.
// This can be used to manually trigger a poll outside the planned intervals,
// for example after a failed deployment or to check for new commits after a network outage.
func (h *handlerData) TriggerPollHandler(w http.ResponseWriter, r *http.Request) {
	// Add a job id to the context to track deployments in the logs
	jobID := id.New()
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
	defer func() {
		_ = r.Body.Close()
	}()

	wait, ok := getBoolQueryParam(r, w, jobLog, jobID, "wait", true)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.appConfig.MaxPayloadSize)

	decoder := json.NewDecoder(r.Body)

	var pollConfigs []poll.Config
	if err := decoder.Decode(&pollConfigs); err != nil {
		h.pollDecodeError(w, jobID, err)

		return
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("request body must contain a single JSON value")
		}

		h.pollDecodeError(w, jobID, err)

		return
	}

	jobID, err := h.runPollConfigs(r.Context(), pollConfigs, wait, jobLog)
	if err != nil {
		if isLifecycleCancellation(err) {
			JSONError(w, err.Error(), "", jobID, http.StatusServiceUnavailable)

			return
		}

		var runsFailed *pollRunsFailedError
		if wait && errors.As(err, &runsFailed) {
			JSONError(w, err.Error(), "", jobID, http.StatusInternalServerError)

			return
		}

		errMsg := err.Error()

		var validationErr *pollConfigValidationError
		if errors.As(err, &validationErr) {
			errMsg = fmt.Sprintf("invalid poll configuration at index %d", validationErr.Index)
		}

		jobLog.Error(errMsg, logger.ErrAttr(err))
		JSONError(w, errMsg, errors.Unwrap(err), jobID, http.StatusBadRequest)

		return
	}

	if wait {
		JSONResponse(w, "poll jobs complete", jobID, http.StatusOK)

		return
	}

	JSONResponse(w, "poll jobs started", jobID, http.StatusAccepted)
}

func (h *handlerData) pollDecodeError(w http.ResponseWriter, jobID string, err error) {
	errMsg := "failed to decode json in body"
	h.log.Error(errMsg, logger.ErrAttr(err))

	status := http.StatusBadRequest

	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		status = http.StatusRequestEntityTooLarge
	}

	JSONError(w, errMsg, err.Error(), jobID, status)
}
