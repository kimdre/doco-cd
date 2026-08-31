package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
)

type triggerPollInput struct {
	Configs []triggerPollConfig `json:"configs" jsonschema:"poll configurations to run"`
	Wait    *bool               `json:"wait,omitempty" jsonschema:"wait for completion; defaults to true"`
}

type triggerPollConfig struct {
	Source       config.SourceType `json:"source,omitempty"`
	SourceURL    string            `json:"url"`
	Reference    string            `json:"reference,omitempty"`
	CustomTarget string            `json:"target,omitempty"`
	Deployments  []*deploy.Config  `json:"deployments,omitempty"`
}

type triggerPollOutput struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

func (h *handlerData) addPollMCPTools(server *mcp.Server) {
	inputSchema := mustToolInputSchema[triggerPollInput]("trigger_poll")
	inputSchema.Properties["configs"].MaxItems = new(maxTriggerPollConfigs)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "trigger_poll",
		Description: "Trigger one or more poll configurations immediately. Prefer wait=false and poll get_deployment_run - long wait=true calls are cancelled if the server shuts down (10s grace).",
		Annotations: destructiveMCPAnnotations(false),
		InputSchema: inputSchema,
	}, instrumentMCPTool(h.log, "trigger_poll", h.triggerPollTool))
}

func (h *handlerData) triggerPollTool(ctx context.Context, _ *mcp.CallToolRequest, input triggerPollInput) (*mcp.CallToolResult, triggerPollOutput, error) {
	wait := valueOr(input.Wait, true)

	configs := make([]poll.Config, len(input.Configs))
	for i, cfg := range input.Configs {
		configs[i] = poll.Config{
			Source:       cfg.Source,
			SourceUrl:    cfg.SourceURL,
			Reference:    cfg.Reference,
			CustomTarget: cfg.CustomTarget,
			Deployments:  cfg.Deployments,
		}
	}

	jobID, err := h.controlPlaneRuns.TriggerPoll(ctx, configs, wait, h.log.Logger)
	result, status := triggerRunToolResult(wait, err)

	return result, triggerPollOutput{JobID: jobID, Status: status}, nil
}
