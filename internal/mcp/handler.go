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
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

// RunOperations is the control-plane surface consumed by the MCP server.
type RunOperations interface {
	List(limit int, trigger, status string) []controlplane.Run
	Get(jobID string) (controlplane.Run, bool)
	ListScheduledJobs(ctx context.Context, contextName, stackName string) ([]controlplane.ScheduledJobInfo, error)
	TriggerScheduledJob(ctx context.Context, jobID, contextName, jobName, stackName string, wait bool) (string, error)
	TriggerPoll(ctx context.Context, configs []poll.Config, wait bool, log *slog.Logger) (string, error)
	TriggerMirrorCompaction(ctx context.Context, jobID string, req controlplane.MirrorCompactionRequest, wait bool) (string, error)
}

// Dependencies contains the runtime services and HTTP settings required by Handler.
type Dependencies struct {
	Version              string          `validate:"required"`
	APIKeys              restapi.Keyring `validate:"required"`
	MaxPayloadSize       int64           `validate:"min=1"`
	TrustedProxyHeader   string          `default:"X-Forwarded-For"`
	TrustedProxyNetworks []netip.Prefix
	Logger               *logger.Logger          `validate:"required,nostructlevel"`
	DockerCLI            command.Cli             `validate:"required,nostructlevel"`
	Contexts             *docker.ContextRegistry `validate:"required,nostructlevel"`
	Runs                 RunOperations           `validate:"required,nostructlevel"`
	// SyncWindows is the sync-window policy. Nil means no windows.
	SyncWindows *syncwindow.Policy `validate:"-"`
}

// Handler serves the stateless MCP HTTP transport and owns all MCP tools.
type Handler struct {
	apiKeys              restapi.Keyring
	trustedProxyHeader   string
	trustedProxyNetworks []netip.Prefix
	log                  *logger.Logger
	dockerCli            command.Cli
	contexts             *docker.ContextRegistry
	controlPlaneRuns     RunOperations
	syncWindows          *syncwindow.Policy
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

	if len(dependencies.APIKeys) == 0 {
		return nil, errors.New("mcp API keys are required")
	}

	if err := dependencies.APIKeys.Validate(); err != nil {
		return nil, fmt.Errorf("validate MCP API keys: %w", err)
	}

	return newHandler(dependencies), nil
}

type roleContextKey struct{}

// newHandler registers MCP tools and builds the authenticated stateless HTTP transport.
func newHandler(dependencies Dependencies) *Handler {
	h := &Handler{
		apiKeys:              append(restapi.Keyring(nil), dependencies.APIKeys...),
		trustedProxyHeader:   strings.TrimSpace(dependencies.TrustedProxyHeader),
		trustedProxyNetworks: append([]netip.Prefix(nil), dependencies.TrustedProxyNetworks...),
		log: &logger.Logger{
			Logger: dependencies.Logger.With(slog.String("component", "mcp")),
			Level:  dependencies.Logger.Level,
		},
		dockerCli:        dependencies.DockerCLI,
		contexts:         dependencies.Contexts,
		controlPlaneRuns: dependencies.Runs,
		syncWindows:      dependencies.SyncWindows,
	}

	mcpLogger := slog.New(&sdkLogHandler{next: h.log.Handler()})
	servers := make(map[restapi.Role]*sdkmcp.Server, 3)

	for _, role := range []restapi.Role{restapi.RoleRead, restapi.RoleWrite, restapi.RoleFull} {
		server := sdkmcp.NewServer(
			&sdkmcp.Implementation{Name: "doco-cd", Version: dependencies.Version},
			&sdkmcp.ServerOptions{Logger: mcpLogger},
		)
		h.registerTools(server, role)
		servers[role] = server
	}

	h.transport = sdkmcp.NewStreamableHTTPHandler(func(r *http.Request) *sdkmcp.Server {
		role, _ := r.Context().Value(roleContextKey{}).(restapi.Role)

		return servers[role]
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

// registerTools installs the tools available to a role.
func (h *Handler) registerTools(server *sdkmcp.Server, role restapi.Role) {
	if !role.Allows(restapi.RoleRead) {
		return
	}

	h.addHealthTool(server)
	h.addRunTools(server)
	h.addScheduledJobReadTool(server)
	h.addSyncWindowTool(server)
	h.addProjectReadTools(server)
	h.addStackReadTools(server)

	if role.Allows(restapi.RoleWrite) {
		h.addProjectTools(server, role)
		h.addStackTools(server, role)
		h.addScheduledJobTriggerTool(server)
		h.addPollTool(server)
		h.addStorageTools(server)
	}
}

// ServeHTTP authenticates a request before forwarding it to the MCP transport.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	role, ok := h.apiKeys.Authenticate(r)
	if !ok {
		h.log.Error(restapi.ErrInvalidApiKey.Error(), slog.String("ip", h.requestIP(r)))
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)

		return
	}

	h.transport.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), roleContextKey{}, role)))
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
