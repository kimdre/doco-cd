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

const defaultProjectActionTimeout = 30

type controlProjectInput struct {
	ProjectName string `json:"project_name" jsonschema:"Compose project name"`
	Action      string `json:"action" jsonschema:"project action: start, stop, or restart"`
	Timeout     *int   `json:"timeout,omitempty" jsonschema:"timeout in seconds; defaults to 30"`
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
}

type destroyProjectOutput struct {
	ProjectName string `json:"project_name"`
	Status      string `json:"status"`
	Volumes     bool   `json:"volumes"`
	Images      bool   `json:"images"`
}

func (h *handlerData) addProjectMCPTools(server *mcp.Server) {
	controlSchema, err := jsonschema.For[controlProjectInput](nil)
	if err != nil {
		panic(fmt.Sprintf("infer control_project input schema: %v", err))
	}

	controlSchema.Properties["action"].Enum = []any{"start", "stop", "restart"}

	destructive := true
	closedWorld := false

	mcp.AddTool(server, &mcp.Tool{
		Name:        "control_project",
		Description: "Start, stop, or restart a Docker Compose project.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: &destructive,
			IdempotentHint:  false,
			OpenWorldHint:   &closedWorld,
		},
		InputSchema: controlSchema,
	}, instrumentMCPTool(h.log, "control_project", h.controlProject))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "destroy_project",
		Description: "Destroy a Docker Compose project and optionally remove its volumes and images. Reconciliation-managed projects may be restored automatically by drift recovery.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: &destructive,
			IdempotentHint:  true,
			OpenWorldHint:   &closedWorld,
		},
	}, instrumentMCPTool(h.log, "destroy_project", h.destroyProjectTool))
}

func (h *handlerData) controlProject(ctx context.Context, _ *mcp.CallToolRequest, input controlProjectInput) (*mcp.CallToolResult, controlProjectOutput, error) {
	projectName := strings.TrimSpace(input.ProjectName)
	if projectName == "" {
		return nil, controlProjectOutput{}, errors.New("missing project name")
	}

	timeout := defaultProjectActionTimeout
	if input.Timeout != nil {
		timeout = *input.Timeout
	}

	jobLog := h.log.With(slog.String("mcp_tool", "control_project"))

	result, err := h.runProjectAction(ctx, projectName, input.Action, timeout, jobLog)
	if err != nil {
		return nil, controlProjectOutput{}, err
	}

	return nil, controlProjectOutput{ProjectName: result.ProjectName, Action: result.Action, Status: "completed"}, nil
}

func (h *handlerData) destroyProjectTool(ctx context.Context, _ *mcp.CallToolRequest, input destroyProjectInput) (*mcp.CallToolResult, destroyProjectOutput, error) {
	projectName := strings.TrimSpace(input.ProjectName)
	if projectName == "" {
		return nil, destroyProjectOutput{}, errors.New("missing project name")
	}

	timeout := defaultProjectActionTimeout
	if input.Timeout != nil {
		timeout = *input.Timeout
	}

	removeVolumes := true
	if input.Volumes != nil {
		removeVolumes = *input.Volumes
	}

	removeImages := true
	if input.Images != nil {
		removeImages = *input.Images
	}

	jobLog := h.log.With(slog.String("mcp_tool", "destroy_project"))

	result, err := h.destroyProject(ctx, projectName, timeout, removeVolumes, removeImages, jobLog)
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
