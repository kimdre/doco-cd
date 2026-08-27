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
	Stack   string `json:"stack,omitempty" jsonschema:"optional stack or Compose project name filter"`
	Context string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type listScheduledJobsOutput struct {
	Jobs []scheduler.JobInfo `json:"jobs"`
}

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

type mcpPortSummary struct {
	TargetPort    int64  `json:"target_port"`
	PublishedPort int64  `json:"published_port"`
	Protocol      string `json:"protocol,omitempty"`
	Mode          string `json:"mode,omitempty"`
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
	contextClient, err := h.resolveMCPDockerContext(ctx, input.Context)
	if err != nil {
		return nil, listScheduledJobsOutput{}, err
	}

	stackName := strings.TrimSpace(input.Stack)

	var jobs []scheduler.JobInfo
	if h.scheduler != nil {
		jobs, err = h.scheduler.ListJobs(ctx, contextClient.Name, stackName)
	} else {
		jobs, err = scheduler.ListJobs(ctx, contextClient.Cli, stackName)
	}

	if err != nil {
		return nil, listScheduledJobsOutput{}, fmt.Errorf("failed to list scheduled jobs: %w", err)
	}

	return nil, listScheduledJobsOutput{Jobs: jobs}, nil
}

func (h *handlerData) listProjects(ctx context.Context, _ *mcp.CallToolRequest, input listProjectsInput) (*mcp.CallToolResult, listProjectsOutput, error) {
	contextClient, err := h.resolveMCPDockerContext(ctx, input.Context)
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

func (h *handlerData) getProject(ctx context.Context, _ *mcp.CallToolRequest, input getProjectInput) (*mcp.CallToolResult, getProjectOutput, error) {
	projectName := strings.TrimSpace(input.ProjectName)
	if projectName == "" {
		return nil, getProjectOutput{}, errors.New("missing project name")
	}

	contextClient, err := h.resolveMCPDockerContext(ctx, input.Context)
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

func (h *handlerData) listStacks(ctx context.Context, _ *mcp.CallToolRequest, input listStacksInput) (*mcp.CallToolResult, listStacksOutput, error) {
	contextClient, err := h.resolveMCPSwarmDockerContext(ctx, input.Context)
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

func (h *handlerData) getStack(ctx context.Context, _ *mcp.CallToolRequest, input getStackInput) (*mcp.CallToolResult, getStackOutput, error) {
	stackName := strings.TrimSpace(input.StackName)
	if stackName == "" {
		return nil, getStackOutput{}, errors.New("missing stack name")
	}

	contextClient, err := h.resolveMCPSwarmDockerContext(ctx, input.Context)
	if err != nil {
		return nil, getStackOutput{}, err
	}

	services, err := swarm.GetStackServices(ctx, contextClient.Cli.Client(), stackName)
	if err != nil {
		return nil, getStackOutput{}, fmt.Errorf("failed to get stack: %s: %w", stackName, err)
	}

	if len(services) == 0 {
		return nil, getStackOutput{}, fmt.Errorf("stack not found: %s", stackName)
	}

	return nil, getStackOutput{Services: summarizeServices(services)}, nil
}

func summarizeProjects(projects []api.Stack) []mcpProjectSummary {
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

func summarizeContainers(containers []api.ContainerSummary) []mcpContainerSummary {
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

func (h *handlerData) resolveMCPDockerContext(ctx context.Context, contextName string) (docker.ContextClient, error) {
	contextName = docker.NormalizeContextName(contextName)
	if h.contexts != nil {
		contextClient, err := h.contexts.Get(ctx, contextName)
		if err != nil {
			return docker.ContextClient{}, fmt.Errorf("failed to resolve Docker context %q: %w", docker.DisplayContextName(contextName), err)
		}

		return contextClient, nil
	}

	if contextName != "" {
		return docker.ContextClient{}, fmt.Errorf("unknown Docker context: %s", docker.DisplayContextName(contextName))
	}

	if h.dockerCli == nil {
		return docker.ContextClient{}, errors.New("docker cli is required")
	}

	return docker.ContextClient{Name: "", Cli: h.dockerCli}, nil
}

func (h *handlerData) resolveMCPSwarmDockerContext(ctx context.Context, contextName string) (docker.ContextClient, error) {
	contextClient, err := h.resolveMCPDockerContext(ctx, contextName)
	if err != nil {
		return docker.ContextClient{}, err
	}

	if err := requireMCPDockerSwarm(ctx, contextClient); err != nil {
		return docker.ContextClient{}, err
	}

	return contextClient, nil
}

func requireMCPDockerSwarm(ctx context.Context, contextClient docker.ContextClient) error {
	enabled := contextClient.SwarmMode
	if contextClient.Name == "" {
		var err error

		enabled, err = swarm.ResolveModeEnabled(ctx, contextClient.Cli.Client())
		if err != nil {
			return fmt.Errorf("failed to check Docker Swarm mode: %w", err)
		}
	}

	if !enabled {
		return errors.New("swarm features are disabled or the Docker daemon is not an active swarm manager")
	}

	return nil
}
