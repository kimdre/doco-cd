package mcp

import (
	"context"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/docker"
)

type getHealthInput struct{}

type getHealthOutput struct {
	Status string `json:"status"`
}

// addHealthTool registers Docker API health reporting.
func (h *Handler) addHealthTool(server *sdkmcp.Server) {
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "get_health",
		Description: "Verify that doco-cd can access the Docker API.",
		Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true},
	}, instrumentTool(h.log, "get_health", getHealth))
}

// getHealth verifies Docker API access for the current application.
func getHealth(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	_ getHealthInput,
) (*sdkmcp.CallToolResult, getHealthOutput, error) {
	err, errType := docker.VerifyDockerAPIAccessContext(ctx)
	if err != nil {
		return nil, getHealthOutput{}, fmt.Errorf("%w: %w", errType, err)
	}

	return nil, getHealthOutput{Status: "healthy"}, nil
}
