package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker"
	prometheusmetrics "github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/restapi"
)

const mcpPath = "/mcp"

type getHealthInput struct{}

type getHealthOutput struct {
	Status string `json:"status"`
}

func (h *handlerData) newMCPHandler(c *app.Config) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "doco-cd", Version: app.Version}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_health",
		Description: "Verify that doco-cd can access the Docker API.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, instrumentMCPTool("get_health", getHealth))

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		PropagateRequestCancellation: true,
		DisableLocalhostProtection:   true,
		MaxRequestBodyBytes:          c.MaxPayloadSize,
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

func getHealth(context.Context, *mcp.CallToolRequest, getHealthInput) (*mcp.CallToolResult, getHealthOutput, error) {
	err, _ := docker.VerifyDockerAPIAccess()
	if err != nil {
		return nil, getHealthOutput{}, err
	}

	return nil, getHealthOutput{Status: "healthy"}, nil
}

func instrumentMCPTool[In, Out any](tool string, handler mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, request *mcp.CallToolRequest, input In) (*mcp.CallToolResult, Out, error) {
		started := time.Now()

		prometheusmetrics.McpRequestsTotal.WithLabelValues(tool).Inc()

		result, output, err := handler(ctx, request, input)

		prometheusmetrics.McpRequestDuration.WithLabelValues(tool).Observe(time.Since(started).Seconds())

		if err != nil || result != nil && result.IsError {
			prometheusmetrics.McpErrorsTotal.WithLabelValues(tool).Inc()
		}

		return result, output, err
	}
}
