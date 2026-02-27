package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/kimdre/doco-cd/internal/config"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
)

// registerApiEndpoints registers the API endpoints based on the application configuration and
// returns a list of all enabled endpoints.
func registerApiEndpoints(c *config.AppConfig, h *handlerData, log *logger.Logger, mux *http.ServeMux) []string {
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
			{apiPath + "/projects", h.GetProjectsApiHandler},
			{apiPath + "/project/{projectName}", h.ProjectApiHandler},
			{apiPath + "/project/{projectName}/{action}", h.ProjectActionApiHandler},
			{apiPath + "/stacks", h.GetStacksApiHandler},
			{apiPath + "/stack/{stackName}", h.StackApiHandler},
			{apiPath + "/stack/{stackName}/{action}", h.StackActionApiHandler},
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

// HealthCheckHandler handles health check requests.
func (h *handlerData) HealthCheckHandler(w http.ResponseWriter, _ *http.Request) {
	var (
		err     error
		errType error
	)

	jobID := uuid.Must(uuid.NewV7()).String()

	metadata := notification.Metadata{
		JobID:      jobID,
		Repository: "healthcheck",
		Stack:      "",
		Revision:   "",
	}

	err, errType = docker.VerifyDockerAPIAccess()
	if err != nil {
		onError(w, h.log.With(logger.ErrAttr(err)), errType.Error(), err.Error(), http.StatusServiceUnavailable, metadata)

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
			errMsg = "'" + key + "' parameter must be true or false"
			log.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, err, errMsg, jobID, http.StatusBadRequest)

			return defaultVal
		}

		return value
	case "int":
		value, err := strconv.Atoi(queryParam)
		if err != nil {
			err = fmt.Errorf("%w: %s", ErrInvalidParam, key)
			errMsg = "'" + key + "' parameter must be a integer"
			log.With(logger.ErrAttr(err)).Error(errMsg)
			JSONError(w, err, errMsg, jobID, http.StatusBadRequest)

			return defaultVal
		}

		return value
	case "string":
		return queryParam
	default:
		err := errors.New("invalid key type")
		errMsg = "key type must be 'bool', 'int' or 'string'"
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
	jobID := uuid.Must(uuid.NewV7()).String()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", r.RemoteAddr))

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
			errMsg = "failed to get project: " + projectName
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
			errMsg = "failed to remove project: " + projectName
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
	jobID := uuid.Must(uuid.NewV7()).String()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", r.RemoteAddr))

	jobLog.Debug("received api request")

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	showAll := getQueryParam(r, w, jobLog, jobID, "all", "bool", false).(bool)

	projects, err := docker.GetProjects(ctx, h.dockerCli, showAll)
	if err != nil {
		errMsg = "failed to get projects"
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
	jobID := uuid.Must(uuid.NewV7()).String()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", r.RemoteAddr))

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
		errMsg = "failed to get project: " + projectName
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
			errMsg = "failed to start project"
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
			errMsg = "failed to stop project"
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
			errMsg = "failed to restart project"
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
	jobID := uuid.Must(uuid.NewV7()).String()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", r.RemoteAddr))

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
		errMsg = "failed to get stack: " + stackName
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
			errMsg = "'replicas' parameter is required and must be a non-negative integer"
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

				errMsg = "failed to scale service"
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
			err = docker.RestartService(ctx, h.dockerClient, svcName)
			if err != nil {
				if errors.Is(err, docker.ErrJobServiceRestartNotSupported) {
					jobLog.Debug("skipping restart for job-mode service", slog.String("service", svcName))
					continue
				}

				errMsg = "failed to restart service"
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

			err = docker.RerunJobService(ctx, h.dockerClient, svcName)
			if err != nil {
				if errors.Is(err, docker.ErrNotAJobService) {
					jobLog.Debug("skipping non-job service for run action", slog.String("service", svcName))
					continue
				}

				errMsg = "failed to retrigger job service"
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
	jobID := uuid.Must(uuid.NewV7()).String()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", r.RemoteAddr))

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
			errMsg = "failed to get stack: " + stackName
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
			errMsg = "failed to remove stack: " + stackName
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
	jobID := uuid.Must(uuid.NewV7()).String()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", r.RemoteAddr))

	jobLog.Debug("received api request")

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	stacks, err := swarm.GetStacks(ctx, h.dockerCli.Client())
	if err != nil {
		errMsg = "failed to get stacks"
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
