package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/docker/cli/cli/command"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/common/defaults"
	"github.com/kimdre/doco-cd/internal/common/validation"
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

// Dependencies contains the runtime services and HTTP settings required by Handler.
type Dependencies struct {
	Version              string `validate:"required"`
	APISecret            string `validate:"required"`
	MaxPayloadSize       int64  `validate:"min=1"`
	TrustedProxyHeader   string `default:"X-Forwarded-For"`
	TrustedProxyNetworks []netip.Prefix
	Logger               *logger.Logger          `validate:"required,nostructlevel"`
	DockerCLI            command.Cli             `validate:"required,nostructlevel"`
	Contexts             *docker.ContextRegistry `validate:"required,nostructlevel"`
	Runs                 RunOperations           `validate:"required,nostructlevel"`
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

// NewHandler applies defaults, validates dependencies, and constructs the MCP handler.
func NewHandler(dependencies Dependencies) (*Handler, error) {
	dependencies.Version = strings.TrimSpace(dependencies.Version)
	dependencies.TrustedProxyHeader = strings.TrimSpace(dependencies.TrustedProxyHeader)

	// Apply defaults to settings only; defaults.Set recursively traverses structs
	// and must not walk injected service implementations.
	defaultValues := Dependencies{TrustedProxyHeader: dependencies.TrustedProxyHeader}
	if err := defaults.Set(&defaultValues); err != nil {
		return nil, fmt.Errorf("set MCP dependency defaults: %w", err)
	}

	dependencies.TrustedProxyHeader = defaultValues.TrustedProxyHeader

	if err := validation.Validate(dependencies); err != nil {
		return nil, fmt.Errorf("validate MCP dependencies: %w", err)
	}

	if dependencies.Logger.Logger == nil {
		return nil, errors.New("mcp logger is required")
	}

	return newHandler(dependencies), nil
}

// newHandler registers MCP tools and builds the authenticated stateless HTTP transport.
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

// registerTools installs the complete MCP tool catalog on server.
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

// ServeHTTP authenticates a request before forwarding it to the MCP transport.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !restapi.ValidateApiKey(r, h.apiSecret) {
		h.log.Error(restapi.ErrInvalidApiKey.Error(), slog.String("ip", h.requestIP(r)))
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)

		return
	}

	h.transport.ServeHTTP(w, r)
}

// requestIP resolves the client address while honoring configured trusted proxies.
func (h *Handler) requestIP(r *http.Request) string {
	return restapi.ResolveRequestIP(
		r.RemoteAddr,
		h.trustedProxyHeader,
		r.Header,
		h.trustedProxyNetworks,
	)
}
