package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/controlplane"
)

type listDeploymentRunsInput struct {
	Limit   *int   `json:"limit,omitempty" jsonschema:"maximum number of runs to return; defaults to 50 and is capped at 200"`
	Status  string `json:"status,omitempty" jsonschema:"optional run status filter: accepted, running, succeeded, failed, or skipped"`
	Trigger string `json:"trigger,omitempty" jsonschema:"optional run trigger filter: webhook, poll, or scheduled_job"`
}

type listDeploymentRunsOutput struct {
	Runs []controlplane.Run `json:"runs"`
}

type getDeploymentRunInput struct {
	JobID string `json:"job_id" jsonschema:"deployment run job ID"`
}

type getDeploymentRunOutput struct {
	Run controlplane.Run `json:"run"`
}

func (h *Handler) addRunTools(server *sdkmcp.Server) {
	readOnly := &sdkmcp.ToolAnnotations{ReadOnlyHint: true}
	listSchema := mustToolInputSchema[listDeploymentRunsInput]("list_deployment_runs")
	listSchema.Properties["limit"].Minimum = new(1.0)

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "list_deployment_runs",
		Description: "List recent deployment runs, optionally filtered by status and trigger.",
		Annotations: readOnly,
		InputSchema: listSchema,
	}, instrumentTool(h.log, "list_deployment_runs", h.listDeploymentRuns))
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "get_deployment_run",
		Description: "Get one deployment run by job ID.",
		Annotations: readOnly,
	}, instrumentTool(h.log, "get_deployment_run", h.getDeploymentRun))
}

func (h *Handler) listDeploymentRuns(
	_ context.Context,
	_ *sdkmcp.CallToolRequest,
	input listDeploymentRunsInput,
) (*sdkmcp.CallToolResult, listDeploymentRunsOutput, error) {
	limit := 50

	if input.Limit != nil {
		if *input.Limit < 1 {
			return nil, listDeploymentRunsOutput{}, errors.New("invalid parameter: limit: 'limit' parameter must be a positive integer")
		}

		limit = min(*input.Limit, 200)
	}

	status, err := controlplane.NormalizeRunStatus(input.Status)
	if err != nil {
		return nil, listDeploymentRunsOutput{}, err
	}

	trigger, err := controlplane.NormalizeRunTrigger(input.Trigger)
	if err != nil {
		return nil, listDeploymentRunsOutput{}, err
	}

	return nil, listDeploymentRunsOutput{Runs: h.controlPlaneRuns.List(limit, trigger, status)}, nil
}

func (h *Handler) getDeploymentRun(
	_ context.Context,
	_ *sdkmcp.CallToolRequest,
	input getDeploymentRunInput,
) (*sdkmcp.CallToolResult, getDeploymentRunOutput, error) {
	jobID := strings.TrimSpace(input.JobID)
	if jobID == "" {
		return nil, getDeploymentRunOutput{}, errors.New("missing job id")
	}

	run, ok := h.controlPlaneRuns.Get(jobID)
	if !ok {
		return nil, getDeploymentRunOutput{}, fmt.Errorf("run not found: %s", jobID)
	}

	return nil, getDeploymentRunOutput{Run: run}, nil
}
