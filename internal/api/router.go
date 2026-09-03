package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/swaggest/swgui/v5emb"

	"github.com/kimdre/doco-cd/internal/config/app"
)

const (
	// APIPath is the root path for authenticated REST operations.
	APIPath = "/v1/api"
	// WebhookPath is the root path for deployment webhooks.
	WebhookPath = "/v1/webhook"
	// HealthPath is the unauthenticated health endpoint.
	HealthPath = "/v1/health"
	// MCPPath is the stateless MCP transport endpoint.
	MCPPath = "/mcp"
	// OpenAPIRestPath serves the runtime-generated REST and health OpenAPI document.
	OpenAPIRestPath = "/openapi/rest.json"
	// OpenAPIWebhookPath serves the runtime-generated webhook OpenAPI document.
	OpenAPIWebhookPath = "/openapi/webhooks.json"
	// DocsPath serves the embedded Swagger UI for REST and health endpoints.
	DocsPath = "/docs/"
	// DocsWebhookPath serves the embedded Swagger UI for webhook endpoints.
	DocsWebhookPath = "/docs/webhooks/"
)

// Mounts supplies protocol handlers owned outside the API package.
type Mounts struct {
	Webhook http.Handler
	MCP     http.Handler
}

// Operation binds an HTTP method to its OpenAPI operation metadata.
type Operation struct {
	Method    string
	Operation *openapi3.Operation
}

// Route is the shared source of truth for HTTP registration and OpenAPI generation.
type Route struct {
	Pattern    string
	Handler    http.Handler
	Enabled    func(*app.Config) bool
	Root       string
	Operations []Operation
}

// RegisterRoutes registers endpoints based on the application configuration
// and returns all enabled endpoint roots in registration order.
func RegisterRoutes(mux *http.ServeMux, h *Handler, mounts Mounts) ([]string, error) {
	if mux == nil {
		return nil, errors.New("api router is required")
	}

	if h == nil {
		return nil, errors.New("api handler is required")
	}

	if h.appConfig == nil {
		return nil, errors.New("api application configuration is required")
	}

	if h.log == nil {
		return nil, errors.New("api logger is required")
	}

	builder := newSchemaBuilder()

	routes, err := createRouteCatalog(h, mounts, builder)
	if err != nil {
		return nil, fmt.Errorf("create API route catalog: %w", err)
	}

	var enabledEndpoints []string

	seenRoots := make(map[string]bool)

	for _, route := range routes {
		if route.Enabled == nil || !route.Enabled(h.appConfig) {
			continue
		}

		if route.Handler == nil {
			return nil, fmt.Errorf("handler for enabled route %q is required", route.Pattern)
		}

		mux.Handle(route.Pattern, route.Handler)
		h.log.Debug("register API endpoint", slog.String("path", route.Pattern))

		if !seenRoots[route.Root] {
			enabledEndpoints = append(enabledEndpoints, route.Root)
			seenRoots[route.Root] = true
		}
	}

	if h.appConfig.ApiSecret == "" {
		h.log.Info("api endpoints disabled, no api secret configured")
	}

	if h.appConfig.WebhookSecret == "" {
		h.log.Info("webhook endpoints disabled, no webhook secret configured")
	}

	if h.appConfig.OpenAPIEnabled {
		_, restDocumentJSON, err := buildOpenAPIDocument(routesByRoot(routes, HealthPath, APIPath), builder.components)
		if err != nil {
			return nil, fmt.Errorf("build runtime REST OpenAPI document: %w", err)
		}

		_, webhookDocumentJSON, err := buildOpenAPIDocument(routesByRoot(routes, WebhookPath), builder.components)
		if err != nil {
			return nil, fmt.Errorf("build runtime webhook OpenAPI document: %w", err)
		}

		mux.Handle("GET "+OpenAPIRestPath, openAPIDocumentHandler(restDocumentJSON))
		mux.Handle("GET "+OpenAPIWebhookPath, openAPIDocumentHandler(webhookDocumentJSON))
		mux.Handle("GET "+DocsPath, v5emb.New("Doco-CD REST API", OpenAPIRestPath, DocsPath))
		mux.Handle("GET "+DocsWebhookPath, v5emb.New("Doco-CD Webhook API", OpenAPIWebhookPath, DocsWebhookPath))

		enabledEndpoints = append(enabledEndpoints, OpenAPIRestPath, OpenAPIWebhookPath, DocsPath, DocsWebhookPath)
	}

	return enabledEndpoints, nil
}

func routesByRoot(routes []Route, roots ...string) []Route {
	included := make(map[string]bool, len(roots))
	for _, root := range roots {
		included[root] = true
	}

	result := make([]Route, 0, len(routes))
	for _, route := range routes {
		if included[route.Root] && len(route.Operations) > 0 {
			result = append(result, route)
		}
	}

	return result
}

func openAPIDocumentHandler(document []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", openAPIMediaType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(document)
	})
}
