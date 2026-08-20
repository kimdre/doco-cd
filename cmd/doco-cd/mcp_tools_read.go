package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/google/jsonschema-go/jsonschema"
	dockerswarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/scheduler"
)

type listDeploymentRunsInput struct {
	Limit   *int   `json:"limit,omitempty" jsonschema:"maximum number of runs to return; defaults to 50 and is capped at 200"`
	Status  string `json:"status,omitempty" jsonschema:"optional run status filter: accepted, running, succeeded, failed, or skipped"`
	Trigger string `json:"trigger,omitempty" jsonschema:"optional run trigger filter: webhook, poll, or scheduled_job"`
}

type listDeploymentRunsOutput struct {
	Runs []deploymentRun `json:"runs"`
}

type getDeploymentRunInput struct {
	JobID string `json:"job_id" jsonschema:"deployment run job ID"`
}

type getDeploymentRunOutput struct {
	Run deploymentRun `json:"run"`
}

type listScheduledJobsInput struct {
	Stack string `json:"stack,omitempty" jsonschema:"optional stack or Compose project name filter"`
}

type listScheduledJobsOutput struct {
	Jobs []scheduler.JobInfo `json:"jobs"`
}

type listProjectsInput struct {
	All bool `json:"all,omitempty" jsonschema:"include stopped Compose projects"`
}

type listProjectsOutput struct {
	Projects []api.Stack `json:"projects"`
}

type getProjectInput struct {
	ProjectName string `json:"project_name" jsonschema:"Compose project name"`
}

type getProjectOutput struct {
	Containers []api.ContainerSummary `json:"containers"`
}

type listStacksInput struct{}

type listStacksOutput struct {
	Stacks map[string][]dockerswarmtypes.Service `json:"stacks"`
}

type getStackInput struct {
	StackName string `json:"stack_name" jsonschema:"Docker Swarm stack name"`
}

type getStackOutput struct {
	Services []dockerswarmtypes.Service `json:"services"`
}

func (h *handlerData) addReadOnlyMCPTools(server *mcp.Server) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	listDeploymentRunsSchema, err := jsonschema.For[listDeploymentRunsInput](nil)
	if err != nil {
		panic(fmt.Sprintf("infer list_deployment_runs input schema: %v", err))
	}

	listDeploymentRunsSchema.Properties["limit"].Minimum = jsonschema.Ptr(1.0)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_deployment_runs",
		Description: "List recent deployment runs, optionally filtered by status and trigger.",
		Annotations: readOnly,
		InputSchema: listDeploymentRunsSchema,
	}, instrumentMCPTool(h.log, "list_deployment_runs", h.listDeploymentRuns))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_deployment_run",
		Description: "Get one deployment run by job ID.",
		Annotations: readOnly,
	}, instrumentMCPTool(h.log, "get_deployment_run", h.getDeploymentRun))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_scheduled_jobs",
		Description: "List scheduler-managed jobs, optionally filtered by stack or Compose project.",
		Annotations: readOnly,
	}, instrumentMCPTool(h.log, "list_scheduled_jobs", h.listScheduledJobs))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_projects",
		Description: "List Docker Compose projects.",
		Annotations: readOnly,
	}, instrumentMCPTool(h.log, "list_projects", h.listProjects))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_project",
		Description: "Get a Docker Compose project; returns the project's containers.",
		Annotations: readOnly,
	}, instrumentMCPTool(h.log, "get_project", h.getProject))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_stacks",
		Description: "List Docker Swarm stacks and their services.",
		Annotations: readOnly,
	}, instrumentMCPTool(h.log, "list_stacks", h.listStacks))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_stack",
		Description: "Get a Docker Swarm stack and return its services.",
		Annotations: readOnly,
	}, instrumentMCPTool(h.log, "get_stack", h.getStack))
}

func (h *handlerData) listDeploymentRuns(_ context.Context, _ *mcp.CallToolRequest, input listDeploymentRunsInput) (*mcp.CallToolResult, listDeploymentRunsOutput, error) {
	limit := 50

	if input.Limit != nil {
		if *input.Limit < 1 {
			return nil, listDeploymentRunsOutput{}, errors.New("invalid parameter: limit: 'limit' parameter must be a positive integer")
		}

		limit = min(*input.Limit, 200)
	}

	status, err := normalizeDeploymentRunStatus(input.Status)
	if err != nil {
		return nil, listDeploymentRunsOutput{}, err
	}

	trigger, err := normalizeDeploymentRunTrigger(input.Trigger)
	if err != nil {
		return nil, listDeploymentRunsOutput{}, err
	}

	if h.runTracker == nil {
		return nil, listDeploymentRunsOutput{Runs: []deploymentRun{}}, nil
	}

	return nil, listDeploymentRunsOutput{Runs: h.runTracker.List(limit, trigger, status)}, nil
}

func (h *handlerData) getDeploymentRun(_ context.Context, _ *mcp.CallToolRequest, input getDeploymentRunInput) (*mcp.CallToolResult, getDeploymentRunOutput, error) {
	jobID := strings.TrimSpace(input.JobID)
	if jobID == "" {
		return nil, getDeploymentRunOutput{}, errors.New("missing job id")
	}

	if h.runTracker == nil {
		return nil, getDeploymentRunOutput{}, fmt.Errorf("run not found: %s", jobID)
	}

	run, ok := h.runTracker.Get(jobID)
	if !ok {
		return nil, getDeploymentRunOutput{}, fmt.Errorf("run not found: %s", jobID)
	}

	return nil, getDeploymentRunOutput{Run: run}, nil
}

func (h *handlerData) listScheduledJobs(ctx context.Context, _ *mcp.CallToolRequest, input listScheduledJobsInput) (*mcp.CallToolResult, listScheduledJobsOutput, error) {
	if h.dockerCli == nil {
		return nil, listScheduledJobsOutput{}, errors.New("docker cli is required")
	}

	jobs, err := scheduler.ListJobs(ctx, h.dockerCli, strings.TrimSpace(input.Stack))
	if err != nil {
		return nil, listScheduledJobsOutput{}, fmt.Errorf("failed to list scheduled jobs: %w", err)
	}

	return nil, listScheduledJobsOutput{Jobs: jobs}, nil
}

func (h *handlerData) listProjects(ctx context.Context, _ *mcp.CallToolRequest, input listProjectsInput) (*mcp.CallToolResult, listProjectsOutput, error) {
	if h.dockerCli == nil {
		return nil, listProjectsOutput{}, errors.New("docker cli is required")
	}

	projects, err := docker.GetProjects(ctx, h.dockerCli, input.All)
	if err != nil {
		return nil, listProjectsOutput{}, fmt.Errorf("failed to get projects: %w", err)
	}

	if len(projects) == 0 {
		return nil, listProjectsOutput{}, errors.New("no projects found")
	}

	return nil, listProjectsOutput{Projects: projects}, nil
}

func (h *handlerData) getProject(ctx context.Context, _ *mcp.CallToolRequest, input getProjectInput) (*mcp.CallToolResult, getProjectOutput, error) {
	projectName := strings.TrimSpace(input.ProjectName)
	if projectName == "" {
		return nil, getProjectOutput{}, errors.New("missing project name")
	}

	if h.dockerCli == nil {
		return nil, getProjectOutput{}, errors.New("docker cli is required")
	}

	containers, err := docker.GetProjectContainers(ctx, h.dockerCli, projectName)
	if err != nil {
		return nil, getProjectOutput{}, fmt.Errorf("failed to get project: %s: %w", projectName, err)
	}

	if len(containers) == 0 {
		return nil, getProjectOutput{}, fmt.Errorf("project not found: %s", projectName)
	}

	return nil, getProjectOutput{Containers: containers}, nil
}

func (h *handlerData) listStacks(ctx context.Context, _ *mcp.CallToolRequest, _ listStacksInput) (*mcp.CallToolResult, listStacksOutput, error) {
	if err := h.requireSwarmMode(ctx); err != nil {
		return nil, listStacksOutput{}, err
	}

	stacks, err := swarm.GetStacks(ctx, h.dockerCli.Client())
	if err != nil {
		return nil, listStacksOutput{}, fmt.Errorf("failed to get stacks: %w", err)
	}

	if len(stacks) == 0 {
		return nil, listStacksOutput{}, errors.New("no stacks found")
	}

	return nil, listStacksOutput{Stacks: stacks}, nil
}

func (h *handlerData) getStack(ctx context.Context, _ *mcp.CallToolRequest, input getStackInput) (*mcp.CallToolResult, getStackOutput, error) {
	stackName := strings.TrimSpace(input.StackName)
	if stackName == "" {
		return nil, getStackOutput{}, errors.New("missing stack name")
	}

	if err := h.requireSwarmMode(ctx); err != nil {
		return nil, getStackOutput{}, err
	}

	services, err := swarm.GetStackServices(ctx, h.dockerCli.Client(), stackName)
	if err != nil {
		return nil, getStackOutput{}, fmt.Errorf("failed to get stack: %s: %w", stackName, err)
	}

	if len(services) == 0 {
		return nil, getStackOutput{}, fmt.Errorf("stack not found: %s", stackName)
	}

	return nil, getStackOutput{Services: services}, nil
}

func (h *handlerData) requireSwarmMode(ctx context.Context) error {
	if h.dockerCli == nil {
		return errors.New("docker cli is required")
	}

	enabled, err := swarm.ResolveModeEnabled(ctx, h.dockerCli.Client())
	if err != nil {
		return fmt.Errorf("failed to check Docker Swarm mode: %w", err)
	}

	if !enabled {
		return errors.New("swarm features are disabled or the Docker daemon is not an active swarm manager")
	}

	return nil
}
