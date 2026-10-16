package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/common/types/duration"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/logger"
	prometheusmetrics "github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/stages"
)

// mustToolInputSchema derives a tool schema and panics on programmer configuration errors.
func mustToolInputSchema[T any](tool string) *jsonschema.Schema {
	retrySchema, err := jsonschema.For[deploy.RetryConfig](&jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{
			reflect.TypeFor[duration.Duration](): {
				Description: "Non-negative retry delay as whole seconds or a Go duration string.",
				OneOf: []*jsonschema.Schema{
					{Type: "integer", Minimum: new(float64(0))},
					{Type: "string"},
				},
			},
		},
	})
	if err != nil {
		panic(fmt.Sprintf("infer %s retry schema: %v", tool, err))
	}

	retrySchema.Properties["limit"].Minimum = new(float64(0))

	deploymentSchema, err := jsonschema.For[deploy.Config](&jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{
			reflect.TypeFor[deploy.RetryConfig](): retrySchema,
		},
	})
	if err != nil {
		panic(fmt.Sprintf("infer %s deployment schema: %v", tool, err))
	}

	deploymentSchema.Properties["pause"] = &jsonschema.Schema{
		Type:        "boolean",
		Default:     []byte("false"),
		Description: "Freeze deployment updates without stopping workloads. Last-deployed recovery still requires reconciliation.enabled.",
	}

	schema, err := jsonschema.For[T](&jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{
			reflect.TypeFor[deploy.Config]():      deploymentSchema,
			reflect.TypeFor[deploy.RetryConfig](): retrySchema,
		},
	})
	if err != nil {
		panic(fmt.Sprintf("infer %s input schema: %v", tool, err))
	}

	return schema
}

// mustRunToolOutputSchema derives a tool schema and panics on programmer configuration errors.
func mustRunToolOutputSchema[T any](tool string) *jsonschema.Schema {
	schema, err := jsonschema.For[T](&jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{
			reflect.TypeFor[controlplane.RunStatus](): {
				Type: "string",
				Enum: []any{"accepted", "running", "succeeded", "failed", "skipped"},
			},
			reflect.TypeFor[controlplane.RunTrigger](): {
				Type: "string",
				Enum: []any{"webhook", "poll", "scheduled_job", "mirror_compaction"},
			},
		},
	})
	if err != nil {
		panic(fmt.Sprintf("infer %s output schema: %v", tool, err))
	}

	return schema
}

// valueOr returns the value of a pointer or a default value if the pointer is nil.
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
	// Intentional skips preserve their reason without reporting an operational failure.
	if errors.Is(err, stages.ErrSyncWindowBlocked) || errors.Is(err, stages.ErrRetrySuppressed) || errors.Is(err, stages.ErrDeploymentPaused) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: err.Error()}},
		}, string(controlplane.RunStatusSkipped)
	}

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
