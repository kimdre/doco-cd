package mcp

import (
	"context"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/syncwindow"
)

type listSyncWindowsInput struct {
	Repository string `json:"repository,omitempty" jsonschema:"optional repository or artifact name to evaluate, e.g. github.com/acme/app"`
	Deployment string `json:"deployment,omitempty" jsonschema:"optional deployment (stack or project) name to evaluate"`
	Context    string `json:"context,omitempty" jsonschema:"optional Docker context name to evaluate; defaults to default"`
	Manual     bool   `json:"manual,omitempty" jsonschema:"evaluate a manual deployment, which windows with manual_sync let through"`
}

// addSyncWindowTool registers sync-window discovery.
func (h *Handler) addSyncWindowTool(server *sdkmcp.Server) {
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "list_sync_windows",
		Description: "List the configured sync windows and whether each is active. If repository, deployment or context is given, also report whether a deployment of that target is allowed now and when it may deploy next.",
		Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true},
	}, instrumentTool(h.log, "list_sync_windows", h.listSyncWindows))
}

// listSyncWindows reports the sync-window policy and an optional decision.
func (h *Handler) listSyncWindows(
	_ context.Context,
	_ *sdkmcp.CallToolRequest,
	input listSyncWindowsInput,
) (*sdkmcp.CallToolResult, syncwindow.Report, error) {
	target := syncwindow.Target{
		Repository: strings.TrimSpace(input.Repository),
		Deployment: strings.TrimSpace(input.Deployment),
		Context:    strings.TrimSpace(input.Context),
	}

	var targetRef *syncwindow.Target
	if target != (syncwindow.Target{}) {
		targetRef = &target
	}

	origin := syncwindow.OriginAutomatic
	if input.Manual {
		origin = syncwindow.OriginManual
	}

	return nil, h.syncWindows.Report(time.Now(), targetRef, origin), nil
}
