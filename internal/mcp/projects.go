package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
)

type listProjectsInput struct {
	All     bool   `json:"all,omitempty" jsonschema:"include stopped Compose projects"`
	Context string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type listProjectsOutput struct {
	Projects []mcpProjectSummary `json:"projects"`
}

type getProjectInput struct {
	ProjectName string `json:"project_name" jsonschema:"Compose project name"`
	Context     string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type getProjectOutput struct {
	Containers []mcpContainerSummary `json:"containers"`
}

type controlProjectInput struct {
	ProjectName string `json:"project_name" jsonschema:"Compose project name"`
	Action      string `json:"action" jsonschema:"project action: start, stop, or restart"`
	Timeout     *int   `json:"timeout,omitempty" jsonschema:"timeout in seconds; defaults to 30"`
	Context     string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type controlProjectOutput struct {
	ProjectName string `json:"project_name"`
	Action      string `json:"action"`
	Status      string `json:"status"`
}

type destroyProjectInput struct {
	ProjectName string `json:"project_name" jsonschema:"Compose project name"`
	Timeout     *int   `json:"timeout,omitempty" jsonschema:"timeout in seconds; defaults to 30"`
	Volumes     *bool  `json:"volumes,omitempty" jsonschema:"remove project volumes; defaults to true"`
	Images      *bool  `json:"images,omitempty" jsonschema:"remove project images; defaults to true"`
	Context     string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type destroyProjectOutput struct {
	ProjectName string `json:"project_name"`
	Status      string `json:"status"`
	Volumes     bool   `json:"volumes"`
	Images      bool   `json:"images"`
}

type mcpProjectSummary struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type mcpContainerSummary struct {
	ID         string           `json:"id"`
	Name       string           `json:"name"`
	Service    string           `json:"service,omitempty"`
	Image      string           `json:"image"`
	State      string           `json:"state"`
	Status     string           `json:"status"`
	Health     string           `json:"health,omitempty"`
	Publishers []mcpPortSummary `json:"published_ports,omitempty"`
}

type mcpPortSummary struct {
	TargetPort    int64  `json:"target_port"`
	PublishedPort int64  `json:"published_port"`
	Protocol      string `json:"protocol,omitempty"`
	Mode          string `json:"mode,omitempty"`
}

func (h *Handler) addProjectReadTools(server *sdkmcp.Server) {
	readOnly := &sdkmcp.ToolAnnotations{ReadOnlyHint: true}

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "list_projects",
		Description: "List Docker Compose projects.",
		Annotations: readOnly,
	}, instrumentTool(h.log, "list_projects", h.listProjects))
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "get_project",
		Description: "Get a Docker Compose project; returns the project's containers.",
		Annotations: readOnly,
	}, instrumentTool(h.log, "get_project", h.getProject))
}

func (h *Handler) addProjectTools(server *sdkmcp.Server) {
	controlSchema := mustToolInputSchema[controlProjectInput]("control_project")
	controlSchema.Properties["action"].Enum = []any{"start", "stop", "restart"}
	setProjectTimeoutSchema(controlSchema)

	destroySchema := mustToolInputSchema[destroyProjectInput]("destroy_project")
	setProjectTimeoutSchema(destroySchema)

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "control_project",
		Description: "Start, stop, or restart a Docker Compose project.",
		Annotations: destructiveAnnotations(false),
		InputSchema: controlSchema,
	}, instrumentTool(h.log, "control_project", h.controlProject))
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "destroy_project",
		Description: "Destroy a Docker Compose project and optionally remove its volumes and images. Reconciliation-managed projects may be restored automatically by drift recovery.",
		Annotations: destructiveAnnotations(true),
		InputSchema: destroySchema,
	}, instrumentTool(h.log, "destroy_project", h.destroyProject))
}

func setProjectTimeoutSchema(schema *jsonschema.Schema) {
	timeoutSchema := schema.Properties["timeout"]
	timeoutSchema.Minimum = new(1.0)
	timeoutSchema.Maximum = new(float64(controlplane.MaxProjectActionTimeout))
}

func (h *Handler) listProjects(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	input listProjectsInput,
) (*sdkmcp.CallToolResult, listProjectsOutput, error) {
	contextClient, err := h.resolveDockerContext(ctx, input.Context)
	if err != nil {
		return nil, listProjectsOutput{}, err
	}

	projects, err := docker.GetProjects(ctx, contextClient.Cli, input.All)
	if err != nil {
		return nil, listProjectsOutput{}, fmt.Errorf("failed to get projects: %w", err)
	}

	if len(projects) == 0 {
		return nil, listProjectsOutput{}, errors.New("no projects found")
	}

	return nil, listProjectsOutput{Projects: summarizeProjects(projects)}, nil
}

func (h *Handler) getProject(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	input getProjectInput,
) (*sdkmcp.CallToolResult, getProjectOutput, error) {
	projectName := strings.TrimSpace(input.ProjectName)
	if projectName == "" {
		return nil, getProjectOutput{}, errors.New("missing project name")
	}

	contextClient, err := h.resolveDockerContext(ctx, input.Context)
	if err != nil {
		return nil, getProjectOutput{}, err
	}

	containers, err := docker.GetProjectContainers(ctx, contextClient.Cli, projectName)
	if err != nil {
		return nil, getProjectOutput{}, fmt.Errorf("failed to get project: %s: %w", projectName, err)
	}

	if len(containers) == 0 {
		return nil, getProjectOutput{}, fmt.Errorf("project not found: %s", projectName)
	}

	return nil, getProjectOutput{Containers: summarizeContainers(containers)}, nil
}

func (h *Handler) controlProject(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	input controlProjectInput,
) (*sdkmcp.CallToolResult, controlProjectOutput, error) {
	projectName := strings.TrimSpace(input.ProjectName)
	if projectName == "" {
		return nil, controlProjectOutput{}, errors.New("missing project name")
	}

	timeout := valueOr(input.Timeout, controlplane.DefaultProjectActionTimeout)
	jobLog := h.log.With(slog.String("mcp_tool", "control_project"))

	contextClient, err := h.resolveDockerContext(ctx, input.Context)
	if err != nil {
		return nil, controlProjectOutput{}, err
	}

	result, err := controlplane.RunProjectAction(ctx, contextClient.Cli, projectName, input.Action, timeout, jobLog)
	if err != nil {
		return nil, controlProjectOutput{}, err
	}

	return nil, controlProjectOutput{ProjectName: result.ProjectName, Action: result.Action, Status: "completed"}, nil
}

func (h *Handler) destroyProject(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	input destroyProjectInput,
) (*sdkmcp.CallToolResult, destroyProjectOutput, error) {
	projectName := strings.TrimSpace(input.ProjectName)
	if projectName == "" {
		return nil, destroyProjectOutput{}, errors.New("missing project name")
	}

	timeout := valueOr(input.Timeout, controlplane.DefaultProjectActionTimeout)
	removeVolumes := valueOr(input.Volumes, true)
	removeImages := valueOr(input.Images, true)
	jobLog := h.log.With(slog.String("mcp_tool", "destroy_project"))

	contextClient, err := h.resolveDockerContext(ctx, input.Context)
	if err != nil {
		return nil, destroyProjectOutput{}, err
	}

	result, err := controlplane.DestroyProject(ctx, contextClient.Cli, projectName, timeout, removeVolumes, removeImages, jobLog)
	if err != nil {
		return nil, destroyProjectOutput{}, err
	}

	return nil, destroyProjectOutput{
		ProjectName: result.ProjectName,
		Status:      "destroyed",
		Volumes:     result.Volumes,
		Images:      result.Images,
	}, nil
}

func summarizeProjects(projects []composeapi.Stack) []mcpProjectSummary {
	summaries := make([]mcpProjectSummary, len(projects))
	for i, project := range projects {
		summaries[i] = mcpProjectSummary{
			ID:     project.ID,
			Name:   project.Name,
			Status: project.Status,
		}
	}

	return summaries
}

func summarizeContainers(containers []composeapi.ContainerSummary) []mcpContainerSummary {
	summaries := make([]mcpContainerSummary, len(containers))
	for i, container := range containers {
		publishers := make([]mcpPortSummary, len(container.Publishers))
		for j, publisher := range container.Publishers {
			publishers[j] = mcpPortSummary{
				TargetPort:    int64(publisher.TargetPort),
				PublishedPort: int64(publisher.PublishedPort),
				Protocol:      publisher.Protocol,
			}
		}

		summaries[i] = mcpContainerSummary{
			ID:         container.ID,
			Name:       container.Name,
			Service:    container.Service,
			Image:      container.Image,
			State:      string(container.State),
			Status:     container.Status,
			Health:     string(container.Health),
			Publishers: publishers,
		}
	}

	return summaries
}
