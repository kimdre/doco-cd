package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

const testMCPPath = "/mcp"

type apiKeyRoundTripper struct {
	apiKey string
	base   http.RoundTripper
}

func (rt apiKeyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clonedRequest := req.Clone(req.Context())
	clonedRequest.Header.Set(restapi.KeyHeader, rt.apiKey)

	return rt.base.RoundTrip(clonedRequest)
}

type testScheduledJobOperations struct {
	listJobs   func(context.Context, string, string) ([]scheduler.JobInfo, error)
	triggerNow func(context.Context, string, string, string, *secretprovider.SecretProvider) (string, error)
}

func (f testScheduledJobOperations) ListJobs(ctx context.Context, contextName, stackName string) ([]scheduler.JobInfo, error) {
	if f.listJobs == nil {
		return []scheduler.JobInfo{}, nil
	}

	return f.listJobs(ctx, contextName, stackName)
}

func (f testScheduledJobOperations) TriggerNow(
	ctx context.Context,
	contextName string,
	jobName string,
	stackName string,
	secretProvider *secretprovider.SecretProvider,
) (string, error) {
	if f.triggerNow == nil {
		return "", nil
	}

	return f.triggerNow(ctx, contextName, jobName, stackName, secretProvider)
}

type testControlPlaneRunsOptions struct {
	applicationCtx    context.Context
	log               *logger.Logger
	scheduledJobs     controlplane.ScheduledJobOperations
	appConfig         *app.Config
	dataMountPoint    container.MountPoint
	dockerCli         command.Cli
	contexts          *docker.ContextRegistry
	secretProvider    *secretprovider.SecretProvider
	pollRunner        controlplane.PollRunner
	maxRunsPerTrigger map[controlplane.RunTrigger]int
}

func newTestControlPlaneRuns(t testing.TB, options testControlPlaneRunsOptions) *controlplane.Runs {
	t.Helper()

	if options.applicationCtx == nil {
		options.applicationCtx = t.Context()
	}

	if options.log == nil {
		options.log = logger.New(logger.LevelCritical)
	}

	if options.appConfig == nil {
		options.appConfig = &app.Config{}
	}

	if options.scheduledJobs == nil {
		options.scheduledJobs = testScheduledJobOperations{}
	}

	if options.pollRunner == nil {
		options.pollRunner = func(
			context.Context,
			poll.Config,
			*app.Config,
			container.MountPoint,
			command.Cli,
			*docker.ContextRegistry,
			*slog.Logger,
			notification.Metadata,
			*secretprovider.SecretProvider,
			string,
		) error {
			return nil
		}
	}

	runs := controlplane.NewRuns(options.applicationCtx, options.log.Logger, controlplane.Dependencies{
		MaxRunsPerTrigger: options.maxRunsPerTrigger,
		ScheduledJobs:     options.scheduledJobs,
		SecretProvider:    options.secretProvider,
		Poll: controlplane.PollDependencies{
			AppConfig:      options.appConfig,
			DataMountPoint: options.dataMountPoint,
			DockerCLI:      options.dockerCli,
			Contexts:       options.contexts,
			Runner:         options.pollRunner,
		},
	})
	t.Cleanup(runs.CloseAndWait)

	return runs
}

func waitForDeploymentRunStatus(t *testing.T, runs RunOperations, jobID string, want controlplane.RunStatus) controlplane.Run {
	t.Helper()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	for {
		run, ok := runs.Get(jobID)
		if ok && run.Status == want {
			return run
		}

		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("timed out waiting for run %q status %q; last run: %#v", jobID, want, run)
		}
	}
}

func newMCPTestServer(
	t *testing.T,
	enabled bool,
	apiSecret string,
	maxPayloadSize int64,
) (*httptest.Server, []string) {
	t.Helper()

	h := &Handler{log: logger.New(logger.LevelCritical)}

	return newMCPTestServerWithHandler(t, enabled, apiSecret, maxPayloadSize, h)
}

func newMCPTestServerWithHandler(
	t *testing.T,
	enabled bool,
	apiSecret string,
	maxPayloadSize int64,
	handler *Handler,
) (*httptest.Server, []string) {
	t.Helper()

	if handler.controlPlaneRuns == nil {
		handler.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			dockerCli: handler.dockerCli,
			contexts:  handler.contexts,
			log:       handler.log,
		})
	}

	mux := http.NewServeMux()

	var enabledEndpoints []string

	if enabled && apiSecret != "" {
		mcpHandler := newHandler(Dependencies{
			Version:        app.Version,
			APISecret:      apiSecret,
			MaxPayloadSize: maxPayloadSize,
			Logger:         handler.log,
			DockerCLI:      handler.dockerCli,
			Contexts:       handler.contexts,
			Runs:           handler.controlPlaneRuns,
		})
		mux.Handle("POST "+testMCPPath, mcpHandler)
		enabledEndpoints = append(enabledEndpoints, testMCPPath)
	}

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server, enabledEndpoints
}

func callMCPTool(
	t *testing.T,
	session *sdkmcp.ClientSession,
	name string,
	arguments any,
) *sdkmcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}

	if result.IsError {
		t.Fatalf("%s returned a tool error: %#v", name, result.Content)
	}

	return result
}

func assertMCPToolError(
	t *testing.T,
	session *sdkmcp.ClientSession,
	name string,
	arguments any,
	contains string,
) {
	t.Helper()

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}

	if !result.IsError {
		t.Fatalf("expected %s tool error, got %#v", name, result)
	}

	encoded, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(contains)) {
		t.Fatalf("expected %s error to contain %q, got %s", name, contains, encoded)
	}
}

func decodeMCPStructuredContent(t *testing.T, result *sdkmcp.CallToolResult, output any) {
	t.Helper()

	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(encoded, output); err != nil {
		t.Fatalf("decode structured content %s: %v", encoded, err)
	}
}

func skipWithoutLiveDocker(t *testing.T) {
	t.Helper()

	if os.Getenv("DOCO_CD_TEST_FAKE_DOCKER") != "" {
		t.Skip("requires a live Docker daemon")
	}
}

func connectMCPTestClient(t *testing.T, server *httptest.Server) *sdkmcp.ClientSession {
	t.Helper()

	httpClient := server.Client()
	httpClient.Transport = apiKeyRoundTripper{apiKey: testMCPAPIKey, base: httpClient.Transport}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "doco-cd-test", Version: "test"}, nil)

	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{
		Endpoint:             server.URL + testMCPPath,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close MCP session: %v", err)
		}
	})

	return session
}
