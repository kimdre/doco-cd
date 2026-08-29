package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	prometheusmetrics "github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/restapi"
)

const mcpPath = "/mcp"

type getHealthInput struct{}

type getHealthOutput struct {
	Status string `json:"status"`
}

func (h *handlerData) newMCPHandler(c *app.Config) http.Handler {
	// Suppress the verbose MCP server connection logs
	mcpLogLevel := h.log.Level
	if h.log.Level == slog.LevelInfo {
		mcpLogLevel = slog.LevelWarn
	}

	mcpLogger := h.log.WithLevel(mcpLogLevel)
	server := mcp.NewServer(&mcp.Implementation{Name: "doco-cd", Version: app.Version}, &mcp.ServerOptions{Logger: mcpLogger})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_health",
		Description: "Verify that doco-cd can access the Docker API.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, instrumentMCPTool(h.log, "get_health", getHealth))
	h.addReadOnlyMCPTools(server)
	h.addProjectMCPTools(server)
	h.addStackMCPTools(server)
	h.addScheduledJobMCPTools(server)
	h.addPollMCPTools(server)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		PropagateRequestCancellation: true,
		// Same-host reverse proxies are supported, and every request requires API-key authentication.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        c.MaxPayloadSize,
		Logger:                     mcpLogger,
	})

	return h.requireMCPAPIKey(c, handler)
}

func (h *handlerData) requireMCPAPIKey(c *app.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !restapi.ValidateApiKey(r, c.ApiSecret) {
			h.log.Error(restapi.ErrInvalidApiKey.Error(), slog.String("ip", h.requestIP(r)))
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(w, r)
	})
}

func getHealth(ctx context.Context, _ *mcp.CallToolRequest, _ getHealthInput) (*mcp.CallToolResult, getHealthOutput, error) {
	err, errType := docker.VerifyDockerAPIAccessContext(ctx)
	if err != nil {
		return nil, getHealthOutput{}, fmt.Errorf("%w: %w", errType, err)
	}

	return nil, getHealthOutput{Status: "healthy"}, nil
}

// mustToolInputSchema infers the JSON schema for an MCP tool input type.
// It panics on failure, which can only happen at startup for invalid struct tags.
func mustToolInputSchema[T any](tool string) *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("infer %s input schema: %v", tool, err))
	}

	return schema
}

// valueOr returns the value p points to, or def when p is nil.
func valueOr[T any](p *T, def T) T {
	if p != nil {
		return *p
	}

	return def
}

// destructiveMCPAnnotations returns tool annotations for a state-changing, closed-world MCP tool.
func destructiveMCPAnnotations(idempotent bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		DestructiveHint: new(true),
		IdempotentHint:  idempotent,
		OpenWorldHint:   new(false),
	}
}

// triggerRunToolResult maps a trigger outcome to an MCP tool result and deployment run status.
// Operational errors are reported as structured MCP tool errors so callers keep the deployment job ID.
func triggerRunToolResult(wait bool, err error) (*mcp.CallToolResult, string) {
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
		}, string(deploymentRunStatusFailed)
	}

	if wait {
		return nil, string(deploymentRunStatusSucceeded)
	}

	return nil, string(deploymentRunStatusAccepted)
}

func instrumentMCPTool[In, Out any](log *logger.Logger, tool string, handler mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, request *mcp.CallToolRequest, input In) (*mcp.CallToolResult, Out, error) {
		started := time.Now()

		prometheusmetrics.McpRequestsTotal.WithLabelValues(tool).Inc()

		result, output, err := handler(ctx, request, input)

		prometheusmetrics.McpRequestDuration.WithLabelValues(tool).Observe(time.Since(started).Seconds())

		if err != nil || (result != nil && result.IsError) {
			prometheusmetrics.McpErrorsTotal.WithLabelValues(tool).Inc()

			if err != nil {
				log.Error("MCP tool call failed", slog.String("tool", tool), logger.ErrAttr(err))
			} else {
				log.Error("MCP tool call failed", slog.String("tool", tool))
			}
		}

		return result, output, err
	}
}
