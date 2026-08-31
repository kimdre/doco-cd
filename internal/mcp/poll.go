package mcp

import (
	"context"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
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

func (h *Handler) addPollTool(server *sdkmcp.Server) {
	inputSchema := mustToolInputSchema[triggerPollInput]("trigger_poll")
	inputSchema.Properties["configs"].MaxItems = new(controlplane.MaxTriggerPollConfigs)

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "trigger_poll",
		Description: "Trigger one or more poll configurations immediately. Prefer wait=false and poll get_deployment_run - long wait=true calls are cancelled if the server shuts down (10s grace).",
		Annotations: destructiveAnnotations(false),
		InputSchema: inputSchema,
	}, instrumentTool(h.log, "trigger_poll", h.triggerPoll))
}

func (h *Handler) triggerPoll(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	input triggerPollInput,
) (*sdkmcp.CallToolResult, triggerPollOutput, error) {
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
