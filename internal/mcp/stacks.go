package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	dockerswarmtypes "github.com/moby/moby/api/types/swarm"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

type listStacksInput struct {
	Context string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type listStacksOutput struct {
	Stacks map[string][]mcpServiceSummary `json:"stacks"`
}

type getStackInput struct {
	StackName string `json:"stack_name" jsonschema:"Docker Swarm stack name"`
	Context   string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type getStackOutput struct {
	Services []mcpServiceSummary `json:"services"`
}

type controlStackInput struct {
	StackName string `json:"stack_name" jsonschema:"Docker Swarm stack name"`
	Action    string `json:"action" jsonschema:"stack action: scale, restart, or run"`
	Replicas  *int   `json:"replicas,omitempty" jsonschema:"replica count; required for scale"`
	Service   string `json:"service,omitempty" jsonschema:"optional service name within the stack"`
	Wait      *bool  `json:"wait,omitempty" jsonschema:"wait for scaled services; defaults to true"`
	Context   string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type controlStackOutput struct {
	StackName string                           `json:"stack_name"`
	Action    string                           `json:"action"`
	Results   []controlplane.StackActionResult `json:"results"`
}

type removeStackInput struct {
	StackName string `json:"stack_name" jsonschema:"Docker Swarm stack name"`
	Context   string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type removeStackOutput struct {
	StackName string `json:"stack_name"`
	Status    string `json:"status"`
}

type mcpServiceSummary struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Image        string           `json:"image,omitempty"`
	Mode         string           `json:"mode"`
	DesiredTasks uint64           `json:"desired_tasks"`
	RunningTasks uint64           `json:"running_tasks"`
	UpdateState  string           `json:"update_state,omitempty"`
	Publishers   []mcpPortSummary `json:"published_ports,omitempty"`
}

func (h *Handler) addStackReadTools(server *sdkmcp.Server) {
	readOnly := &sdkmcp.ToolAnnotations{ReadOnlyHint: true}

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "list_stacks",
		Description: "List Docker Swarm stacks and their services.",
		Annotations: readOnly,
	}, instrumentTool(h.log, "list_stacks", h.listStacks))
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "get_stack",
		Description: "Get a Docker Swarm stack and return its services.",
		Annotations: readOnly,
	}, instrumentTool(h.log, "get_stack", h.getStack))
}

func (h *Handler) addStackTools(server *sdkmcp.Server) {
	controlSchema := mustToolInputSchema[controlStackInput]("control_stack")
	controlSchema.Properties["action"].Enum = []any{"scale", "restart", "run"}
	controlSchema.Properties["replicas"].Minimum = new(0.0)

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "control_stack",
		Description: "Scale, restart, or run services in a Docker Swarm stack. Returns one result per matching service, including skipped services.",
		Annotations: destructiveAnnotations(false),
		InputSchema: controlSchema,
	}, instrumentTool(h.log, "control_stack", h.controlStack))
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "remove_stack",
		Description: "Remove a Docker Swarm stack. Removal may be partial if Docker returns an error after deleting some resources.",
		Annotations: destructiveAnnotations(true),
	}, instrumentTool(h.log, "remove_stack", h.removeStack))
}

func (h *Handler) listStacks(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	input listStacksInput,
) (*sdkmcp.CallToolResult, listStacksOutput, error) {
	contextClient, err := h.resolveSwarmDockerContext(ctx, input.Context)
	if err != nil {
		return nil, listStacksOutput{}, err
	}

	stacks, err := swarm.GetStacks(ctx, contextClient.Cli.Client())
	if err != nil {
		return nil, listStacksOutput{}, fmt.Errorf("failed to get stacks: %w", err)
	}

	if len(stacks) == 0 {
		return nil, listStacksOutput{}, errors.New("no stacks found")
	}

	return nil, listStacksOutput{Stacks: summarizeStacks(stacks)}, nil
}

func (h *Handler) getStack(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	input getStackInput,
) (*sdkmcp.CallToolResult, getStackOutput, error) {
	stackName := strings.TrimSpace(input.StackName)
	if stackName == "" {
		return nil, getStackOutput{}, errors.New("missing stack name")
	}

	contextClient, err := h.resolveSwarmDockerContext(ctx, input.Context)
	if err != nil {
		return nil, getStackOutput{}, err
	}

	services, err := controlplane.GetStackServices(ctx, contextClient.Cli, stackName)
	if err != nil {
		if errors.Is(err, controlplane.ErrStackNotFound) {
			return nil, getStackOutput{}, err
		}

		return nil, getStackOutput{}, fmt.Errorf("failed to get stack: %s: %w", stackName, err)
	}

	return nil, getStackOutput{Services: summarizeServices(services)}, nil
}

func (h *Handler) controlStack(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	input controlStackInput,
) (*sdkmcp.CallToolResult, controlStackOutput, error) {
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

	contextClient, err := h.resolveSwarmDockerContext(ctx, input.Context)
	if err != nil {
		return nil, controlStackOutput{}, err
	}

	wait := valueOr(input.Wait, true)
	jobLog := h.log.With(slog.String("mcp_tool", "control_stack"))

	results, err := controlplane.RunStackAction(
		ctx,
		contextClient.Cli,
		stackName,
		input.Action,
		strings.TrimSpace(input.Service),
		replicas,
		wait,
		jobLog,
	)
	output := controlStackOutput{StackName: stackName, Action: input.Action, Results: results}

	if err != nil {
		_, isServiceNotFound := errors.AsType[*controlplane.StackServiceNotFoundError](err)
		_, isActionError := errors.AsType[*controlplane.StackServiceActionError](err)

		if isServiceNotFound || isActionError || errors.Is(err, controlplane.ErrNoApplicableStackServices) {
			return &sdkmcp.CallToolResult{
				IsError: true,
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: err.Error()}},
			}, output, nil
		}

		return nil, controlStackOutput{}, err
	}

	return nil, output, nil
}

func (h *Handler) removeStack(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	input removeStackInput,
) (*sdkmcp.CallToolResult, removeStackOutput, error) {
	stackName := strings.TrimSpace(input.StackName)
	if stackName == "" {
		return nil, removeStackOutput{}, errors.New("missing stack name")
	}

	contextClient, err := h.resolveSwarmDockerContext(ctx, input.Context)
	if err != nil {
		return nil, removeStackOutput{}, err
	}

	jobLog := h.log.With(slog.String("mcp_tool", "remove_stack"))
	if err := controlplane.RemoveStack(ctx, contextClient.Cli, stackName, jobLog); err != nil {
		return nil, removeStackOutput{}, fmt.Errorf("failed to remove stack: %s: %w", stackName, err)
	}

	return nil, removeStackOutput{StackName: stackName, Status: "removed"}, nil
}

func summarizeStacks(stacks map[string][]dockerswarmtypes.Service) map[string][]mcpServiceSummary {
	summaries := make(map[string][]mcpServiceSummary, len(stacks))
	for stackName, services := range stacks {
		summaries[stackName] = summarizeServices(services)
	}

	return summaries
}

func summarizeServices(services []dockerswarmtypes.Service) []mcpServiceSummary {
	summaries := make([]mcpServiceSummary, len(services))
	for i, service := range services {
		summary := mcpServiceSummary{
			ID:   service.ID,
			Name: service.Spec.Name,
			Mode: serviceMode(service.Spec.Mode),
		}

		if service.Spec.TaskTemplate.ContainerSpec != nil {
			summary.Image = service.Spec.TaskTemplate.ContainerSpec.Image
		}

		if service.ServiceStatus != nil {
			summary.DesiredTasks = service.ServiceStatus.DesiredTasks
			summary.RunningTasks = service.ServiceStatus.RunningTasks
		}

		if service.UpdateStatus != nil {
			summary.UpdateState = string(service.UpdateStatus.State)
		}

		summary.Publishers = make([]mcpPortSummary, len(service.Endpoint.Ports))
		for j, port := range service.Endpoint.Ports {
			summary.Publishers[j] = mcpPortSummary{
				TargetPort:    int64(port.TargetPort),
				PublishedPort: int64(port.PublishedPort),
				Protocol:      string(port.Protocol),
				Mode:          string(port.PublishMode),
			}
		}

		summaries[i] = summary
	}

	return summaries
}

func serviceMode(mode dockerswarmtypes.ServiceMode) string {
	switch {
	case mode.Replicated != nil:
		return "replicated"
	case mode.Global != nil:
		return "global"
	case mode.ReplicatedJob != nil:
		return "replicated_job"
	case mode.GlobalJob != nil:
		return "global_job"
	default:
		return "unknown"
	}
}
