package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/scheduler"
)

type listScheduledJobsInput struct {
	Stack   string `json:"stack,omitempty" jsonschema:"optional stack or Compose project name filter"`
	Context string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type listScheduledJobsOutput struct {
	Jobs []scheduler.JobInfo `json:"jobs"`
}

type triggerScheduledJobInput struct {
	JobName string `json:"job_name" jsonschema:"scheduled job container or service name"`
	Stack   string `json:"stack,omitempty" jsonschema:"optional stack or Compose project name"`
	Wait    *bool  `json:"wait,omitempty" jsonschema:"wait for completion; defaults to true"`
	Context string `json:"context,omitempty" jsonschema:"optional Docker context; defaults to default"`
}

type triggerScheduledJobOutput struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// addScheduledJobReadTool registers scheduled-job discovery.
func (h *Handler) addScheduledJobReadTool(server *sdkmcp.Server) {
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "list_scheduled_jobs",
		Description: "List scheduler-managed jobs, optionally filtered by stack or Compose project.",
		Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true},
	}, instrumentTool(h.log, "list_scheduled_jobs", h.listScheduledJobs))
}

// addScheduledJobTriggerTool registers manual scheduled-job execution.
func (h *Handler) addScheduledJobTriggerTool(server *sdkmcp.Server) {
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "trigger_scheduled_job",
		Description: "Trigger one configured scheduled job immediately. A succeeded result means the trigger operation completed; it does not guarantee workload completion. Prefer wait=false and poll get_deployment_run - long wait=true calls are cancelled if the server shuts down (10s grace).",
		Annotations: destructiveAnnotations(false),
	}, instrumentTool(h.log, "trigger_scheduled_job", h.triggerScheduledJob))
}

// listScheduledJobs returns jobs for an optional Docker context and stack.
func (h *Handler) listScheduledJobs(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	input listScheduledJobsInput,
) (*sdkmcp.CallToolResult, listScheduledJobsOutput, error) {
	contextClient, err := h.resolveDockerContext(ctx, input.Context)
	if err != nil {
		return nil, listScheduledJobsOutput{}, err
	}

	jobs, err := h.controlPlaneRuns.ListScheduledJobs(ctx, contextClient.Name, strings.TrimSpace(input.Stack))
	if err != nil {
		return nil, listScheduledJobsOutput{}, fmt.Errorf("failed to list scheduled jobs: %w", err)
	}

	return nil, listScheduledJobsOutput{Jobs: jobs}, nil
}

// triggerScheduledJob starts a job and reports its control-plane lifecycle state.
func (h *Handler) triggerScheduledJob(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	input triggerScheduledJobInput,
) (*sdkmcp.CallToolResult, triggerScheduledJobOutput, error) {
	jobName := strings.TrimSpace(input.JobName)
	if jobName == "" {
		return nil, triggerScheduledJobOutput{}, errors.New("missing job name")
	}

	wait := valueOr(input.Wait, true)

	contextClient, err := h.resolveDockerContext(ctx, input.Context)
	if err != nil {
		return nil, triggerScheduledJobOutput{}, err
	}

	jobID, err := h.controlPlaneRuns.TriggerScheduledJob(ctx, "", contextClient.Name, jobName, strings.TrimSpace(input.Stack), wait)
	result, status := triggerRunToolResult(wait, err)

	return result, triggerScheduledJobOutput{JobID: jobID, Status: status}, nil
}
