package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type controlStackInput struct {
	StackName string `json:"stack_name" jsonschema:"Docker Swarm stack name"`
	Action    string `json:"action" jsonschema:"stack action: scale, restart, or run"`
	Replicas  *int   `json:"replicas,omitempty" jsonschema:"replica count; required for scale"`
	Service   string `json:"service,omitempty" jsonschema:"optional service name within the stack"`
	Wait      *bool  `json:"wait,omitempty" jsonschema:"wait for scaled services; defaults to true"`
	Context   string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type controlStackOutput struct {
	StackName string              `json:"stack_name"`
	Action    string              `json:"action"`
	Results   []stackActionResult `json:"results"`
}

type removeStackInput struct {
	StackName string `json:"stack_name" jsonschema:"Docker Swarm stack name"`
	Context   string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type removeStackOutput struct {
	StackName string `json:"stack_name"`
	Status    string `json:"status"`
}

func (h *handlerData) addStackMCPTools(server *mcp.Server) {
	controlSchema, err := jsonschema.For[controlStackInput](nil)
	if err != nil {
		panic(fmt.Sprintf("infer control_stack input schema: %v", err))
	}

	controlSchema.Properties["action"].Enum = []any{"scale", "restart", "run"}
	controlSchema.Properties["replicas"].Minimum = jsonschema.Ptr(0.0)

	destructive := true
	closedWorld := false

	mcp.AddTool(server, &mcp.Tool{
		Name:        "control_stack",
		Description: "Scale, restart, or run services in a Docker Swarm stack. Returns one result per matching service, including skipped services.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: &destructive,
			IdempotentHint:  false,
			OpenWorldHint:   &closedWorld,
		},
		InputSchema: controlSchema,
	}, instrumentMCPTool(h.log, "control_stack", h.controlStack))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "remove_stack",
		Description: "Remove a Docker Swarm stack. Removal may be partial if Docker returns an error after deleting some resources.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: &destructive,
			IdempotentHint:  true,
			OpenWorldHint:   &closedWorld,
		},
	}, instrumentMCPTool(h.log, "remove_stack", h.removeStackTool))
}

func (h *handlerData) controlStack(ctx context.Context, _ *mcp.CallToolRequest, input controlStackInput) (*mcp.CallToolResult, controlStackOutput, error) {
	stackName := strings.TrimSpace(input.StackName)
	if stackName == "" {
		return nil, controlStackOutput{}, errors.New("missing stack name")
	}

	replicas := -1
	if input.Replicas != nil {
		replicas = *input.Replicas
	}

	if input.Action == "scale" && replicas < 0 {
		return nil, controlStackOutput{}, errors.New("'replicas' parameter is required and must be a non-negative integer")
	}

	contextClient, err := h.resolveMCPDockerContext(ctx, input.Context)
	if err != nil {
		return nil, controlStackOutput{}, err
	}

	if err := requireMCPDockerSwarm(ctx, contextClient); err != nil {
		return nil, controlStackOutput{}, err
	}

	wait := true
	if input.Wait != nil {
		wait = *input.Wait
	}

	jobLog := h.log.With(slog.String("mcp_tool", "control_stack"))

	results, err := h.runStackAction(ctx, contextClient.Cli, stackName, input.Action, strings.TrimSpace(input.Service), replicas, wait, jobLog)
	output := controlStackOutput{StackName: stackName, Action: input.Action, Results: results}

	if err != nil {
		var (
			serviceNotFound *stackServiceNotFoundError
			actionErr       *stackServiceActionError
		)
		if errors.As(err, &serviceNotFound) || errors.As(err, &actionErr) || errors.Is(err, errNoApplicableStackServices) {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			}, output, nil
		}

		return nil, controlStackOutput{}, err
	}

	return nil, output, nil
}

func (h *handlerData) removeStackTool(ctx context.Context, _ *mcp.CallToolRequest, input removeStackInput) (*mcp.CallToolResult, removeStackOutput, error) {
	stackName := strings.TrimSpace(input.StackName)
	if stackName == "" {
		return nil, removeStackOutput{}, errors.New("missing stack name")
	}

	contextClient, err := h.resolveMCPDockerContext(ctx, input.Context)
	if err != nil {
		return nil, removeStackOutput{}, err
	}

	if err := requireMCPDockerSwarm(ctx, contextClient); err != nil {
		return nil, removeStackOutput{}, err
	}

	jobLog := h.log.With(slog.String("mcp_tool", "remove_stack"))
	if err := h.removeStack(ctx, contextClient.Cli, stackName, jobLog); err != nil {
		return nil, removeStackOutput{}, fmt.Errorf("failed to remove stack: %s: %w", stackName, err)
	}

	return nil, removeStackOutput{StackName: stackName, Status: "removed"}, nil
}
