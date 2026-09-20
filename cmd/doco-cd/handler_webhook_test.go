package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	restserver "github.com/kimdre/doco-cd/internal/api"
	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/controlplane"

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/reconciliation"
	"github.com/kimdre/doco-cd/internal/source"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestPostSkippedWebhookCommitStatusUsesRunContext(t *testing.T) {
	type postedStatus struct {
		State       string `json:"state"`
		Description string `json:"description"`
		Context     string `json:"context"`
	}

	var received postedStatus

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode status: %v", err)
		}

		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	postSkippedWebhookCommitStatus(t.Context(), &app.Config{
		GitCommitStatus: true,
		GitAccessToken:  "token",
		GitScmProvider:  string(commitstatus.ProviderGitea),
		GitScmApiUrl:    config.HttpUrl(server.URL),
	}, logger.New(logger.LevelCritical).Logger, webhook.ParsedPayload{
		Source:    webhook.PayloadSourceGit,
		CommitSHA: plumbing.NewHash("0123456789012345678901234567890123456789"),
		FullName:  "owner/repo",
		CloneURL:  "https://git.example.com/owner/repo.git",
		WebURL:    "https://git.example.com/owner/repo",
	})

	if received.State != string(commitstatus.StateSuccess) || received.Description != "Skipped" {
		t.Fatalf("unexpected skipped status: %+v", received)
	}

	if received.Context != commitstatus.DeployContext {
		t.Fatalf("context = %q, want %q", received.Context, commitstatus.DeployContext)
	}
}

func TestRunWebhookSynchronouslyIgnoresRequestCancellation(t *testing.T) {
	t.Parallel()

	applicationCtx, cancelApplication := context.WithCancel(t.Context())
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{applicationCtx: applicationCtx})
	jobID := runs.Accept("webhook", controlplane.RunTriggerWebhook, controlplane.RunMetadata{})

	requestCtx, cancelRequest := context.WithCancel(t.Context())
	runCtx := make(chan context.Context, 1)
	result := make(chan error, 1)

	go func() {
		result <- runs.Execute(requestCtx, jobID, controlplane.RunExecution{
			Mode:         controlplane.RunSynchronousDetached,
			PanicContext: "webhook deployment",
			PanicError:   errWebhookDeploymentPanicked,
		}, func(ctx context.Context) (controlplane.RunResult, error) {
			runCtx <- ctx

			<-ctx.Done()

			return controlplane.RunResult{}, ctx.Err()
		})
	}()

	ctx := <-runCtx

	cancelRequest()

	if err := ctx.Err(); err != nil {
		t.Fatalf("webhook run cancelled with request: %v", err)
	}

	cancelApplication()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("webhook run error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("webhook run did not stop during application shutdown")
	}

	runs.CloseAndWait()
}

// fakeSourcePreparer returns a canned result for concurrency tests without
// touching disk or the network.
type fakeSourcePreparer struct {
	repoName string
}

func (f fakeSourcePreparer) Prepare(context.Context, source.Request) (source.Result, error) {
	return source.Result{
		SourceType: config.SourceTypeGit,
		RepoName:   f.repoName,
		Revision:   "0123456789012345678901234567890123456789",
	}, nil
}

// fakeContextResolver satisfies controlplane.DockerContextResolver without a
// real Docker daemon; Deployment.Deploy only checks the error return of Get,
// never the resolved docker.ContextClient value itself.
type fakeContextResolver struct{}

func (fakeContextResolver) Get(context.Context, string) (docker.ContextClient, error) {
	return docker.ContextClient{}, nil
}

// fakeReconciler is a minimal controlplane.Reconciler fake letting tests
// observe/control what happens once a webhook event reaches deployment.
// deploy is called synchronously by controlplane.Deployment.Deploy for every event.
type fakeReconciler struct {
	deploy func(ctx context.Context, req reconciliation.DeployRequest) error
}

func (f fakeReconciler) Deploy(ctx context.Context, req reconciliation.DeployRequest) error {
	return f.deploy(ctx, req)
}

// newConcurrencyTestHandler builds an orchestrationHandler whose deployment
// operation is backed entirely by fakes (no source checkout, no Docker
// daemon), so tests can drive real WebhookHandler HTTP requests and observe
// exactly how many reach the reconciler concurrently.
func newConcurrencyTestHandler(t *testing.T, deploy func(ctx context.Context, req reconciliation.DeployRequest) error) orchestrationHandler {
	t.Helper()

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.GitCommitStatus = false

	log := logger.New(logger.LevelCritical)

	mountPoint := container.MountPoint{
		Type:        "bind",
		Source:      t.TempDir(),
		Destination: t.TempDir(),
		Mode:        "rw",
	}

	deployment, err := controlplane.NewDeployment(controlplane.DeploymentDependencies{
		SourcePreparer: fakeSourcePreparer{repoName: "kimdre/doco-cd"},
		Reconciler:     fakeReconciler{deploy: deploy},
		Contexts:       fakeContextResolver{},
		DataMountPoint: mountPoint,
	})
	if err != nil {
		t.Fatalf("failed to create fake deployment operation: %v", err)
	}

	return orchestrationHandler{
		appConfig:  appConfig,
		log:        log,
		testName:   test.ConvertTestName(t.Name()),
		deployment: deployment,
		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			appConfig:      appConfig,
			dataMountPoint: mountPoint,
			log:            log,
		}),
	}
}

// newConcurrencyTestWebhookRequest builds a signed GitHub push webhook
// request for target - a per-request customTarget standing in for "which
// stack this event deploys", since these tests drive WebhookHandler directly
// (bypassing the HTTP mux that would otherwise populate it from the path).
func newConcurrencyTestWebhookRequest(t *testing.T, appConfig *app.Config, target string) *http.Request {
	t.Helper()

	payload, err := os.ReadFile(filepath.Join(WorkingDir, githubPayloadFile))
	if err != nil {
		t.Fatal(err)
	}

	req := newWebhookRequest(t, restserver.WebhookPath+"?wait=true", payload, appConfig)
	req.SetPathValue("customTarget", target)

	return req
}

// TestWebhookHandlerDifferentTargetsRunConcurrently proves that removing the
// former repository-wide webhook lock (acquireWebhookRepoLock/lock.RepoLock)
// lets two webhook events for the same repository, targeting different
// stacks, reach the reconciler at the same time instead of queuing behind one another.
func TestWebhookHandlerDifferentTargetsRunConcurrently(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	started := make(chan string, 2)
	release := make(chan struct{})

	h := newConcurrencyTestHandler(t, func(_ context.Context, req reconciliation.DeployRequest) error {
		started <- req.Metadata.Target

		<-release

		return nil
	})

	stackA := t.Name() + "/stack-a"
	stackB := t.Name() + "/stack-b"
	reqA := newConcurrencyTestWebhookRequest(t, h.appConfig, stackA)
	reqB := newConcurrencyTestWebhookRequest(t, h.appConfig, stackB)

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		h.WebhookHandler(httptest.NewRecorder(), reqA)
	}()

	go func() {
		defer wg.Done()

		h.WebhookHandler(httptest.NewRecorder(), reqB)
	}()

	seen := make(set.Set[string], 2)

	for range 2 {
		select {
		case target := <-started:
			seen.Add(target)
		case <-time.After(time.Second):
			close(release)
			t.Fatal("expected both different-stack webhook events to reach the reconciler without serializing")
		}
	}

	if !seen.Contains(stackA) || !seen.Contains(stackB) {
		close(release)
		t.Fatalf("expected both stacks to reach the reconciler, got %v", seen)
	}

	close(release)
	wg.Wait()
}

// TestWebhookHandlerSameTargetStillSerializesViaStackLock proves that even
// without the removed repository-wide webhook lock, two webhook events for
// the same repository AND the same stack still cannot run concurrently: the
// per-stack lock (internal/lock.LockStack/UnlockStack), applied deep inside
// the real deployment path (internal/docker.Deploy), still serializes them.
// The fake reconciler below stands in for that call site by taking the same
// lock the real one does.
func TestWebhookHandlerSameTargetStillSerializesViaStackLock(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	const simulatedWork = 100 * time.Millisecond

	stackKey := t.Name() + "/shared-stack"

	var (
		mu      sync.Mutex
		overlap bool
		active  bool
	)

	h := newConcurrencyTestHandler(t, func(context.Context, reconciliation.DeployRequest) error {
		lock.LockStack(stackKey)
		defer lock.UnlockStack(stackKey)

		mu.Lock()
		if active {
			overlap = true
		}

		active = true
		mu.Unlock()

		time.Sleep(simulatedWork)

		mu.Lock()
		active = false
		mu.Unlock()

		return nil
	})

	reqA := newConcurrencyTestWebhookRequest(t, h.appConfig, "shared-stack")
	reqB := newConcurrencyTestWebhookRequest(t, h.appConfig, "shared-stack")

	var wg sync.WaitGroup

	wg.Add(2)

	start := time.Now()

	go func() {
		defer wg.Done()

		h.WebhookHandler(httptest.NewRecorder(), reqA)
	}()

	go func() {
		defer wg.Done()

		h.WebhookHandler(httptest.NewRecorder(), reqB)
	}()

	wg.Wait()

	if overlap {
		t.Fatal("expected same-stack webhook events to serialize via the per-stack lock, but they overlapped")
	}

	if elapsed := time.Since(start); elapsed < 2*simulatedWork {
		t.Fatalf("expected same-stack webhook events to run sequentially (>= %v), took %v", 2*simulatedWork, elapsed)
	}
}

const (
	githubPayloadFile          = "testdata/github_payload.json"
	githubPayloadFileSwarmMode = "testdata/github_payload_swarm_mode.json"
	webhookTestPollInterval    = 100 * time.Millisecond
)

// webhookFixtureFiles backs the local, network-independent fixture repo used
// by TestHandlerData_WebhookHandler in non-swarm mode, so that test doesn't
// depend on cloning the live kimdre/doco-cd GitHub repository.
var webhookFixtureFiles = map[string]string{
	".doco-cd.yaml": `
name: webhook-test-deploy
compose_files:
  - test.compose.yaml
`,
	"test.compose.yaml": `
services:
  app:
    image: nginx:latest
    ports:
      - "80"  # use random published port
    volumes:
      - ./:/usr/share/nginx/html
`,
	"index.html": "webhook test fixture index page\n",
}

func newWebhookRequest(t *testing.T, url string, payload []byte, appConfig *app.Config) *http.Request {
	t.Helper()

	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set(webhook.ScmProviderSecurityHeaders[webhook.Github], "sha256="+webhook.GenerateHMAC(payload, appConfig.WebhookSecret))
	req.Header.Set(webhook.ScmProviderEventHeaders[webhook.Github], "push")

	return req
}

func validateWebhookResponse(t *testing.T, rr *httptest.ResponseRecorder, expectedStatusCode int, expectedResponse string, idx int) {
	t.Helper()

	if status := rr.Code; status != expectedStatusCode {
		t.Errorf("handler[%d] returned wrong status code: got %v want %v", idx, status, expectedStatusCode)
	}

	regex, err := regexp.Compile(expectedResponse)
	if err != nil {
		t.Fatal(err)
	}

	if !regex.MatchString(rr.Body.String()) {
		t.Fatalf("handler[%d] returned unexpected body: got %v want %v", idx, rr.Body.String(), expectedResponse)
	}
}

func TestHandlerData_WebhookHandler(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	expectedResponse := `{"content":"job completed successfully","job_id":"[a-f0-9-]{36}"}`
	expectedStatusCode := http.StatusCreated
	tmpDir := t.TempDir()

	stackName := test.ConvertTestName(t.Name())

	payloadFile := githubPayloadFile
	// payloadCloneUrl is the clone_url baked into the webhook payload fixture
	// file. In non-swarm mode it is rewritten below (via SourceURLRewrites) to
	// a local, ephemeral fixture repository so this test doesn't depend on
	// the live kimdre/doco-cd GitHub repository.
	payloadCloneUrl := "https://github.com/kimdre/doco-cd.git"
	indexPath := "index.html"

	var cloneUrl string

	if SwarmModeEnabled {
		payloadFile = githubPayloadFileSwarmMode
		cloneUrl = "https://github.com/kimdre/doco-cd_tests.git"
		indexPath = path.Join("html", "index.html")
	} else {
		_, fixtureCloneURL, _ := newLocalFixtureRepo(t, webhookFixtureFiles)
		cloneUrl = fixtureCloneURL
	}

	// The repository content actually deployed lives under the store's
	// per-revision artifact directory, not directly at repoBaseDir - its
	// exact revision subdirectory name isn't known until after the webhook
	// deploys, so it's resolved via glob below.
	repoBaseDir := path.Join(tmpDir, git.GetRepoName(cloneUrl))

	payload, err := os.ReadFile(filepath.Join(WorkingDir, payloadFile))
	if err != nil {
		t.Fatal(err)
	}

	minifiedPayload := new(bytes.Buffer)

	err = json.Compact(minifiedPayload, payload)
	if err != nil {
		t.Fatal(err)
	}

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.GitCommitStatus = false

	if !SwarmModeEnabled {
		// Route the payload's clone URL to the local fixture repo created
		// above, so this test never clones the live kimdre/doco-cd repo.
		appConfig.SourceURLRewrites = map[string]string{
			payloadCloneUrl: cloneUrl,
		}
	}

	log := logger.New(logger.LevelCritical)

	dockerCli, err := docker.CreateDockerCli(appConfig.DockerQuietDeploy)
	if err != nil {
		t.Fatalf("Failed to create docker client: %v", err)
	}

	dockerClient := dockerCli.Client()

	t.Cleanup(func() {
		err = dockerCli.Client().Close()
		if err != nil {
			return
		}
	})

	mountPoint := container.MountPoint{
		Type:        "bind",
		Source:      tmpDir,
		Destination: tmpDir,
		Mode:        "rw",
	}

	h := orchestrationHandler{
		appConfig: appConfig,
		log:       log,
		testName:  stackName,

		deployment: newTestDeployment(t, appConfig, mountPoint, reconciliation.Dependencies{
			AppConfig:      appConfig,
			DataMountPoint: mountPoint,
			DockerCLI:      dockerCli,
		}),
		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			appConfig:      appConfig,
			dataMountPoint: mountPoint,
			dockerCli:      dockerCli,
			log:            log,
		}),
	}

	req := newWebhookRequest(t, restserver.WebhookPath+"?wait=true", minifiedPayload.Bytes(), appConfig)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(h.WebhookHandler)
	handler.ServeHTTP(rr, req)

	validateWebhookResponse(t, rr, expectedStatusCode, expectedResponse, 0)

	ctx := context.Background()

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		t.Fatalf("failed to create compose service: %v", err)
	}

	downOpts := api.DownOptions{
		RemoveOrphans: true,
		Images:        "local",
		Volumes:       true,
	}

	t.Cleanup(func() {
		if service != nil {
			err = service.Down(ctx, stackName, downOpts)
			if err != nil {
				t.Fatal(err)
			}
		}
	})

	var (
		testContainerID   string
		testContainerPort string
	)

	if SwarmModeEnabled {
		t.Log("Testing in Swarm mode")

		inspectName := stackName + "_" + "test"

		svc, err := waitForSwarmService(ctx, t, dockerClient, inspectName, 30*time.Second)
		if err != nil {
			t.Fatalf("Failed to find swarm service for test container: %v", err)
		}

		if len(svc.Endpoint.Ports) == 0 {
			t.Fatal("Test service has no published ports")
		}

		testContainerPort = strconv.FormatUint(uint64(svc.Endpoint.Ports[0].PublishedPort), 10)

		t.Cleanup(func() {
			_, err = dockerCli.Client().ServiceRemove(ctx, inspectName, client.ServiceRemoveOptions{})
			if err != nil {
				t.Fatalf("Failed to remove test container service: %v", err)
			}
		})
	} else {
		containers, err := test.WaitForStack(ctx, t, service, stackName, 30*time.Second)
		if err != nil {
			t.Fatalf("Failed waiting for stack to be ready: %v", err)
		}

		for _, c := range containers {
			if c.Service == "app" {
				testContainerID = c.ID
				break
			}
		}

		if testContainerID == "" {
			t.Fatal("Test container not found in stack")
		}

		testContainerPort = waitForPublishedContainerPort(ctx, t, dockerCli.Client(), testContainerID, 15*time.Second)
	}

	testURL := "http://127.0.0.1:" + testContainerPort
	t.Logf("Test URL: %s", testURL)

	httpClient := &http.Client{Timeout: 3 * time.Second}

	resp := waitForHTTPStatusOK(ctx, t, httpClient, testURL, 15*time.Second)

	t.Cleanup(
		func() {
			err = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
		})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Test container returned unexpected status code: got %v want %v", resp.StatusCode, http.StatusOK)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	bodyString := string(bodyBytes)

	indexMatches, err := filepath.Glob(filepath.Join(repoBaseDir, "artifacts", "*", indexPath))
	if err != nil || len(indexMatches) == 0 {
		t.Fatalf("failed to locate %s under %s (glob error: %v)", indexPath, repoBaseDir, err)
	}

	fileContent, err := os.ReadFile(indexMatches[0]) // #nosec G304
	if err != nil {
		t.Fatalf("Failed to read index.html file: %v", err)
	}

	if bodyString != string(fileContent) {
		t.Fatalf("Test container returned unexpected body: got '%v' but want '%v'", bodyString, string(fileContent))
	}
}

func waitForPublishedContainerPort(ctx context.Context, t *testing.T, cli client.APIClient, containerID string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)

	portKey, err := network.ParsePort("80/tcp")
	if err != nil {
		t.Fatalf("failed to parse container port: %v", err)
	}

	for {
		testContainer, err := cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		if err == nil {
			networkPort := testContainer.Container.NetworkSettings.Ports[portKey]
			if len(networkPort) > 0 {
				return networkPort[0].HostPort
			}
		}

		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("failed to inspect container: %v", err)
			}

			t.Fatal("test container port not published")
		}

		time.Sleep(webhookTestPollInterval)
	}
}

func waitForHTTPStatusOK(ctx context.Context, t *testing.T, httpClient *http.Client, url string, timeout time.Duration) *http.Response {
	t.Helper()

	deadline := time.Now().Add(timeout)
	attempt := 0

	for {
		attempt++

		resp, err := httpClient.Get(url) // #nosec G107
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				t.Logf("Successfully connected to test container on attempt %d", attempt)
				return resp
			}

			t.Logf("Test container returned status code %d on attempt %d", resp.StatusCode, attempt)

			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		} else {
			t.Logf("Failed to make GET request to test container (attempt %d): %v", attempt, err)
		}

		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("timed out waiting for test container HTTP readiness: %v", err)
			}

			t.Fatalf("timed out waiting for test container HTTP readiness: last status %d", resp.StatusCode)
		}

		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled waiting for test container HTTP readiness: %v", ctx.Err())
		case <-time.After(webhookTestPollInterval):
		}
	}
}

func TestWebhookHandler_WaitQueryParam(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.GitCommitStatus = false

	log := logger.New(logger.LevelCritical)

	mountPoint := container.MountPoint{
		Type:        "bind",
		Source:      t.TempDir(),
		Destination: t.TempDir(),
		Mode:        "rw",
	}

	h := orchestrationHandler{
		appConfig: appConfig,
		log:       log,

		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			appConfig:      appConfig,
			dataMountPoint: mountPoint,
			log:            log,
		}),
	}

	testCases := []struct {
		name string
		url  string
	}{
		{
			name: "Default async when wait not set",
			url:  restserver.WebhookPath,
		},
		{
			name: "Synchronous when wait=true",
			url:  restserver.WebhookPath + "?wait=true",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Provide a payload that fails parsing; wait should not affect parse errors.
			req, err := http.NewRequest("POST", tc.url, bytes.NewReader([]byte("{}")))
			if err != nil {
				t.Fatal(err)
			}

			h.testName = test.ConvertTestName(t.Name())

			rr := httptest.NewRecorder()
			h.WebhookHandler(rr, req)

			if rr.Code == 0 {
				t.Fatalf("expected recorder to have a status code")
			}
		})
	}
}

// waitForSwarmService waits until a swarm service exists (and optionally has published ports).
func waitForSwarmService(ctx context.Context, t *testing.T, cli client.APIClient, serviceName string, timeout time.Duration) (swarmTypes.Service, error) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	var lastErr error

	for time.Now().Before(deadline) {
		result, err := cli.ServiceInspect(ctx, serviceName, client.ServiceInspectOptions{
			InsertDefaults: true,
		})
		if err == nil {
			return result.Service, nil
		}

		lastErr = err

		time.Sleep(500 * time.Millisecond)
	}

	return swarmTypes.Service{}, fmt.Errorf("timed out waiting for service %s after %s: %w", serviceName, timeout.String(), lastErr)
}
