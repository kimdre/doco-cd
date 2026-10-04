package mcp

import (
	"context"
	"errors"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/git"
)

type compactMirrorsInput struct {
	Repository string `json:"repository,omitempty" jsonschema:"only compact the mirrors of this repository, e.g. github.com/acme/app or a clone URL; defaults to all mirrors"`
	Mode       string `json:"mode,omitempty" jsonschema:"repack (default) re-encodes all objects and needs several times the mirror size in memory; copy only concatenates the packfiles"`
	MaxSize    *int64 `json:"max_size,omitempty" jsonschema:"size in bytes above which repack skips a mirror; defaults to 268435456 (256 MiB) and 0 disables the limit"`
	Wait       *bool  `json:"wait,omitempty" jsonschema:"wait for completion; defaults to false"`
}

type compactMirrorsOutput struct {
	JobID   string `json:"job_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// addStorageTools registers storage maintenance.
func (h *Handler) addStorageTools(server *sdkmcp.Server) {
	inputSchema := mustToolInputSchema[compactMirrorsInput]("compact_mirrors")
	inputSchema.Properties["mode"].Enum = []any{string(git.MirrorCompactionRepack), string(git.MirrorCompactionCopy)}
	inputSchema.Properties["max_size"].Minimum = new(0.0)

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name: "compact_mirrors",
		Description: "Compact the Git mirrors in the data directory into one packfile each. " +
			"Mirrors that a deployment holds are skipped, and only one compaction runs at a time. " +
			"Prefer wait=false and poll get_deployment_run; a running compaction is cancelled when the server shuts down.",
		Annotations: &sdkmcp.ToolAnnotations{
			DestructiveHint: new(false),
			IdempotentHint:  true,
			OpenWorldHint:   new(false),
		},
		InputSchema: inputSchema,
	}, instrumentTool(h.log, "compact_mirrors", h.compactMirrors))
}

// compactMirrors starts a mirror compaction run and reports its state.
func (h *Handler) compactMirrors(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	input compactMirrorsInput,
) (*sdkmcp.CallToolResult, compactMirrorsOutput, error) {
	wait := valueOr(input.Wait, false)

	jobID, err := h.controlPlaneRuns.TriggerMirrorCompaction(ctx, "", controlplane.MirrorCompactionRequest{
		Repository:   input.Repository,
		Mode:         git.MirrorCompactionMode(input.Mode),
		MaxSizeBytes: input.MaxSize,
	}, wait)
	if errors.Is(err, controlplane.ErrInvalidMirrorCompactionRequest) || errors.Is(err, controlplane.ErrNoMirrors) {
		return nil, compactMirrorsOutput{}, err
	}

	if _, ok := errors.AsType[*controlplane.MirrorCompactionActiveError](err); ok {
		output := compactMirrorsOutput{JobID: jobID, Status: string(controlplane.RunStatusRunning)}
		if run, ok := h.controlPlaneRuns.Get(jobID); ok {
			output.Status = string(run.Status)
		}

		return &sdkmcp.CallToolResult{
			IsError: true,
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: err.Error()}},
		}, output, nil
	}

	result, status := triggerRunToolResult(wait, err)
	output := compactMirrorsOutput{JobID: jobID, Status: status}

	if run, ok := h.controlPlaneRuns.Get(jobID); ok && wait {
		output.Status = string(run.Status)
		output.Message = run.Message
	}

	return result, output, nil
}
