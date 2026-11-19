package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/logger"
)

func TestRegisterRoutesPreservesEnabledEndpointOrderAndGating(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name          string
		config        app.Config
		wantEndpoints []string
	}{
		{
			name:          "health only",
			wantEndpoints: []string{HealthPath},
		},
		{
			name:          "OpenAPI documentation",
			config:        app.Config{OpenAPIEnabled: true},
			wantEndpoints: []string{HealthPath, OpenAPIRestPath, OpenAPIWebhookPath, DocsPath, DocsWebhookPath},
		},
		{
			name:          "REST API",
			config:        app.Config{APIKeys: testAPIKeys("api-secret")}, // #nosec G101 -- test fixture.
			wantEndpoints: []string{HealthPath, APIPath},
		},
		{
			name: "all mounts",
			config: app.Config{
				APIKeys:        testAPIKeys("api-secret"), // #nosec G101 -- test fixture.
				McpEnabled:     true,
				OpenAPIEnabled: true,
				WebhookSecret:  "webhook-secret", // #nosec G101 -- test fixture.
			},
			wantEndpoints: []string{HealthPath, APIPath, MCPPath, WebhookPath, OpenAPIRestPath, OpenAPIWebhookPath, DocsPath, DocsWebhookPath},
		},
		{
			name: "MCP requires API secret",
			config: app.Config{
				McpEnabled:    true,
				WebhookSecret: "webhook-secret", // #nosec G101 -- test fixture.
			},
			wantEndpoints: []string{HealthPath, WebhookPath},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			h := &Handler{
				appConfig: &testCase.config,
				log:       logger.New(logger.LevelCritical),
			}
			mux := http.NewServeMux()

			got, err := RegisterRoutes(mux, h, Mounts{
				Webhook: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
				MCP:     http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
			})
			if err != nil {
				t.Fatalf("register routes: %v", err)
			}

			if !slices.Equal(got, testCase.wantEndpoints) {
				t.Fatalf("enabled endpoints = %v, want %v", got, testCase.wantEndpoints)
			}
		})
	}
}

func TestRegisterRoutesUsesExpectedPatternsAndOpaqueMounts(t *testing.T) {
	t.Parallel()

	h := &Handler{
		appConfig: &app.Config{
			APIKeys:       testAPIKeys("api-secret"), // #nosec G101 -- test fixture.
			McpEnabled:    true,
			WebhookSecret: "webhook-secret", // #nosec G101 -- test fixture.
		},
		log: logger.New(logger.LevelCritical),
	}

	var webhookPath, mcpMethod string

	mux := http.NewServeMux()

	_, err := RegisterRoutes(mux, h, Mounts{
		Webhook: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			webhookPath = r.URL.Path

			w.WriteHeader(http.StatusNoContent)
		}),
		MCP: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mcpMethod = r.Method

			w.WriteHeader(http.StatusNoContent)
		}),
	})
	if err != nil {
		t.Fatalf("register routes: %v", err)
	}

	for _, pattern := range []string{
		HealthPath,
		APIPath + "/runs",
		APIPath + "/run/{jobID}",
		APIPath + "/jobs",
		APIPath + "/job/{jobName}/run",
		APIPath + "/projects",
		APIPath + "/project/{projectName}",
		APIPath + "/project/{projectName}/start",
		APIPath + "/project/{projectName}/stop",
		APIPath + "/project/{projectName}/restart",
		APIPath + "/project/{projectName}/recreate",
		APIPath + "/stacks",
		APIPath + "/stack/{stackName}",
		APIPath + "/stack/{stackName}/scale",
		APIPath + "/stack/{stackName}/restart",
		APIPath + "/stack/{stackName}/run",
		APIPath + "/poll/run",
		APIPath + "/storage/compact",
		APIPath + "/sync-windows",
		"POST " + MCPPath,
		WebhookPath,
		WebhookPath + "/{customTarget}",
	} {
		request := httptest.NewRequest(http.MethodPost, patternRequestPath(pattern), nil)

		_, matchedPattern := mux.Handler(request)
		if matchedPattern != pattern {
			t.Errorf("request for %q matched %q", pattern, matchedPattern)
		}
	}

	for _, path := range []string{
		APIPath + "/project/project-name/invalid",
		APIPath + "/stack/stack-name/invalid",
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))

		if response.Code != http.StatusNotFound {
			t.Errorf("POST %s status = %d, want 404", path, response.Code)
		}
	}

	webhookResponse := httptest.NewRecorder()
	mux.ServeHTTP(webhookResponse, httptest.NewRequest(http.MethodPost, WebhookPath+"/production", nil))

	if webhookResponse.Code != http.StatusNoContent || webhookPath != WebhookPath+"/production" {
		t.Fatalf("webhook mount response = %d, path = %q", webhookResponse.Code, webhookPath)
	}

	mcpResponse := httptest.NewRecorder()
	mux.ServeHTTP(mcpResponse, httptest.NewRequest(http.MethodPost, MCPPath, nil))

	if mcpResponse.Code != http.StatusNoContent || mcpMethod != http.MethodPost {
		t.Fatalf("MCP mount response = %d, method = %q", mcpResponse.Code, mcpMethod)
	}

	getMCPResponse := httptest.NewRecorder()
	mux.ServeHTTP(getMCPResponse, httptest.NewRequest(http.MethodGet, MCPPath, nil))

	if getMCPResponse.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET MCP status = %d, want %d", getMCPResponse.Code, http.StatusMethodNotAllowed)
	}
}

func patternRequestPath(pattern string) string {
	switch pattern {
	case APIPath + "/run/{jobID}":
		return APIPath + "/run/job-id"
	case APIPath + "/job/{jobName}/run":
		return APIPath + "/job/job-name/run"
	case APIPath + "/project/{projectName}":
		return APIPath + "/project/project-name"
	case APIPath + "/stack/{stackName}":
		return APIPath + "/stack/stack-name"
	case WebhookPath + "/{customTarget}":
		return WebhookPath + "/production"
	case "POST " + MCPPath:
		return MCPPath
	default:
		return strings.NewReplacer("{projectName}", "project-name", "{stackName}", "stack-name").Replace(pattern)
	}
}
