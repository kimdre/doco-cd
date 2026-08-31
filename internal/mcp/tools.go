package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/logger"
	prometheusmetrics "github.com/kimdre/doco-cd/internal/prometheus"
)

// mustToolInputSchema derives a tool schema and panics on programmer configuration errors.
func mustToolInputSchema[T any](tool string) *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("infer %s input schema: %v", tool, err))
	}

	return schema
}

func valueOr[T any](p *T, def T) T {
	if p != nil {
		return *p
	}

	return def
}

// destructiveAnnotations marks a tool as destructive and closed to external systems.
func destructiveAnnotations(idempotent bool) *sdkmcp.ToolAnnotations {
	return &sdkmcp.ToolAnnotations{
		DestructiveHint: new(true),
		IdempotentHint:  idempotent,
		OpenWorldHint:   new(false),
	}
}

// triggerRunToolResult maps run completion into MCP content and a lifecycle status.
func triggerRunToolResult(wait bool, err error) (*sdkmcp.CallToolResult, string) {
	if err != nil {
		return &sdkmcp.CallToolResult{
			IsError: true,
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: err.Error()}},
		}, string(controlplane.RunStatusFailed)
	}

	if wait {
		return nil, string(controlplane.RunStatusSucceeded)
	}

	return nil, string(controlplane.RunStatusAccepted)
}

// instrumentTool records tool latency, request counts, failures, and operational errors.
func instrumentTool[In, Out any](
	log *logger.Logger,
	tool string,
	handler sdkmcp.ToolHandlerFor[In, Out],
) sdkmcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest, input In) (*sdkmcp.CallToolResult, Out, error) {
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
