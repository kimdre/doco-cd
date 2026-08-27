package main

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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

func (h *handlerData) addScheduledJobMCPTools(server *mcp.Server) {
	destructive := true
	closedWorld := false

	mcp.AddTool(server, &mcp.Tool{
		Name:        "trigger_scheduled_job",
		Description: "Trigger one configured scheduled job immediately. A succeeded result means the trigger operation completed; it does not guarantee workload completion. Prefer wait=false and poll get_deployment_run - long wait=true calls are cancelled if the server shuts down (10s grace).",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: &destructive,
			IdempotentHint:  false,
			OpenWorldHint:   &closedWorld,
		},
	}, instrumentMCPTool(h.log, "trigger_scheduled_job", h.triggerScheduledJobTool))
}

func (h *handlerData) triggerScheduledJobTool(ctx context.Context, _ *mcp.CallToolRequest, input triggerScheduledJobInput) (*mcp.CallToolResult, triggerScheduledJobOutput, error) {
	jobName := strings.TrimSpace(input.JobName)
	if jobName == "" {
		return nil, triggerScheduledJobOutput{}, errors.New("missing job name")
	}

	wait := true
	if input.Wait != nil {
		wait = *input.Wait
	}

	contextClient, err := h.resolveMCPDockerContext(ctx, input.Context)
	if err != nil {
		return nil, triggerScheduledJobOutput{}, err
	}

	jobID, err := h.triggerScheduledJobRun(ctx, "", contextClient.Cli, contextClient.Name, jobName, strings.TrimSpace(input.Stack), wait)

	status := deploymentRunStatusAccepted
	if wait {
		status = deploymentRunStatusSucceeded
	}

	output := triggerScheduledJobOutput{JobID: jobID, Status: string(status)}
	if err != nil {
		output.Status = string(deploymentRunStatusFailed)

		return &mcp.CallToolResult{ //nolint:nilerr // Operational errors are returned as structured MCP tool errors with the deployment job ID.
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
		}, output, nil
	}

	return nil, output, nil
}
