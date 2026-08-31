package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/docker/cli/cli/command"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
)

// RunOperations is the control-plane surface consumed by the MCP server.
type RunOperations interface {
	List(limit int, trigger, status string) []controlplane.Run
	Get(jobID string) (controlplane.Run, bool)
	ListScheduledJobs(ctx context.Context, contextName, stackName string) ([]scheduler.JobInfo, error)
	TriggerScheduledJob(ctx context.Context, jobID, contextName, jobName, stackName string, wait bool) (string, error)
	TriggerPoll(ctx context.Context, configs []poll.Config, wait bool, log *slog.Logger) (string, error)
}

type Dependencies struct {
	Version              string
	APISecret            string
	MaxPayloadSize       int64
	TrustedProxyHeader   string
	TrustedProxyNetworks []netip.Prefix
	Logger               *logger.Logger
	DockerCLI            command.Cli
	Contexts             *docker.ContextRegistry
	Runs                 RunOperations
}

// Handler serves the stateless MCP HTTP transport and owns all MCP tools.
type Handler struct {
	apiSecret            string
	trustedProxyHeader   string
	trustedProxyNetworks []netip.Prefix
	log                  *logger.Logger
	dockerCli            command.Cli
	contexts             *docker.ContextRegistry
	controlPlaneRuns     RunOperations
	transport            http.Handler
}

func NewHandler(dependencies Dependencies) (*Handler, error) {
	switch {
	case strings.TrimSpace(dependencies.Version) == "":
		return nil, errors.New("mcp version is required")
	case dependencies.APISecret == "":
		return nil, errors.New("mcp API secret is required")
	case dependencies.MaxPayloadSize < 1:
		return nil, errors.New("mcp max payload size must be positive")
	case dependencies.Logger == nil || dependencies.Logger.Logger == nil:
		return nil, errors.New("mcp logger is required")
	case dependencies.DockerCLI == nil:
		return nil, errors.New("mcp docker CLI is required")
	case dependencies.Contexts == nil:
		return nil, errors.New("mcp docker context registry is required")
	case dependencies.Runs == nil:
		return nil, errors.New("mcp control-plane run operations are required")
	}

	return newHandler(dependencies), nil
}

func newHandler(dependencies Dependencies) *Handler {
	h := &Handler{
		apiSecret:            dependencies.APISecret,
		trustedProxyHeader:   strings.TrimSpace(dependencies.TrustedProxyHeader),
		trustedProxyNetworks: append([]netip.Prefix(nil), dependencies.TrustedProxyNetworks...),
		log:                  dependencies.Logger,
		dockerCli:            dependencies.DockerCLI,
		contexts:             dependencies.Contexts,
		controlPlaneRuns:     dependencies.Runs,
	}

	// Suppress the verbose MCP server connection logs.
	mcpLogLevel := h.log.Level
	if h.log.Level == slog.LevelInfo {
		mcpLogLevel = slog.LevelWarn
	}

	mcpLogger := h.log.WithLevel(mcpLogLevel)
	server := sdkmcp.NewServer(
		&sdkmcp.Implementation{Name: "doco-cd", Version: dependencies.Version},
		&sdkmcp.ServerOptions{Logger: mcpLogger},
	)

	h.registerTools(server)
	h.transport = sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return server
	}, &sdkmcp.StreamableHTTPOptions{
		Stateless:                    true,
		PropagateRequestCancellation: true,
		// Same-host reverse proxies are supported, and every request requires API-key authentication.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        dependencies.MaxPayloadSize,
		Logger:                     mcpLogger,
	})

	return h
}

func (h *Handler) registerTools(server *sdkmcp.Server) {
	h.addHealthTool(server)
	h.addRunTools(server)
	h.addScheduledJobReadTool(server)
	h.addProjectReadTools(server)
	h.addStackReadTools(server)
	h.addProjectTools(server)
	h.addStackTools(server)
	h.addScheduledJobTriggerTool(server)
	h.addPollTool(server)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !restapi.ValidateApiKey(r, h.apiSecret) {
		h.log.Error(restapi.ErrInvalidApiKey.Error(), slog.String("ip", h.requestIP(r)))
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)

		return
	}

	h.transport.ServeHTTP(w, r)
}

func (h *Handler) requestIP(r *http.Request) string {
	return restapi.ResolveRequestIP(
		r.RemoteAddr,
		h.trustedProxyHeader,
		r.Header,
		h.trustedProxyNetworks,
	)
}
