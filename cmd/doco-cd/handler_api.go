package main

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/config/app"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
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
