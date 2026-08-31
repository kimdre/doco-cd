package api

import (
	"log/slog"
	"net/http"
)

const (
	APIPath     = "/v1/api"
	WebhookPath = "/v1/webhook"
	HealthPath  = "/v1/health"
	MCPPath     = "/mcp"
)

type Mounts struct {
	Webhook http.Handler
	MCP     http.Handler
}

// RegisterRoutes registers endpoints based on the application configuration
// and returns all enabled endpoint roots in registration order.
func RegisterRoutes(mux *http.ServeMux, h *Handler, mounts Mounts) []string {
	if mux == nil {
		panic("api router is required")
	}

	if h == nil {
		panic("api handler is required")
	}

	var enabledEndpoints []string

	type endpoint struct {
		path    string
		handler http.Handler
	}

	enabledEndpoints = append(enabledEndpoints, HealthPath)
	mux.Handle(HealthPath, http.HandlerFunc(h.HealthCheckHandler))
	h.log.Debug("register health endpoint", slog.String("path", HealthPath))

	if h.appConfig.ApiSecret != "" {
		enabledEndpoints = append(enabledEndpoints, APIPath)

		endpoints := []endpoint{
			{APIPath + "/runs", http.HandlerFunc(h.GetDeploymentRunsHandler)},
			{APIPath + "/run/{jobID}", http.HandlerFunc(h.GetDeploymentRunHandler)},
			{APIPath + "/jobs", http.HandlerFunc(h.GetScheduledJobsHandler)},
			{APIPath + "/job/{jobName}/run", http.HandlerFunc(h.TriggerScheduledJobHandler)},
			{APIPath + "/projects", http.HandlerFunc(h.GetProjectsApiHandler)},
			{APIPath + "/project/{projectName}", http.HandlerFunc(h.ProjectApiHandler)},
			{APIPath + "/project/{projectName}/{action}", http.HandlerFunc(h.ProjectActionApiHandler)},
			{APIPath + "/stacks", http.HandlerFunc(h.GetStacksApiHandler)},
			{APIPath + "/stack/{stackName}", http.HandlerFunc(h.StackApiHandler)},
			{APIPath + "/stack/{stackName}/{action}", http.HandlerFunc(h.StackActionApiHandler)},
			{APIPath + "/poll/run", http.HandlerFunc(h.TriggerPollHandler)},
		}

		for _, ep := range endpoints {
			mux.Handle(ep.path, ep.handler)
			h.log.Debug("register api endpoint", slog.String("path", ep.path))
		}
	} else {
		h.log.Info("api endpoints disabled, no api secret configured")
	}

	if h.appConfig.McpEnabled && h.appConfig.ApiSecret != "" {
		if mounts.MCP == nil {
			panic("api MCP handler is required when MCP is enabled")
		}

		enabledEndpoints = append(enabledEndpoints, MCPPath)
		mux.Handle("POST "+MCPPath, mounts.MCP)
		h.log.Debug("register MCP endpoint", slog.String("path", MCPPath))
	}

	if h.appConfig.WebhookSecret != "" {
		if mounts.Webhook == nil {
			panic("api webhook handler is required when webhooks are enabled")
		}

		enabledEndpoints = append(enabledEndpoints, WebhookPath)

		endpoints := []endpoint{
			{WebhookPath, mounts.Webhook},
			{WebhookPath + "/{customTarget}", mounts.Webhook},
		}

		for _, ep := range endpoints {
			mux.Handle(ep.path, ep.handler)
			h.log.Debug("register webhook endpoint", slog.String("path", ep.path))
		}
	} else {
		h.log.Info("webhook endpoints disabled, no webhook secret configured")
	}

	return enabledEndpoints
}
