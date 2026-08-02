package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/docker/swarm"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/webhook"
)

const (
	githubPayloadFile          = "testdata/github_payload.json"
	githubPayloadFileSwarmMode = "testdata/github_payload_swarm_mode.json"
	webhookTestPollInterval    = 100 * time.Millisecond
	composeContent             = `services:
  nginx:
    image: nginx:latest
    ports:
      - "80"
`
)

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
	cloneUrl := "https://github.com/kimdre/doco-cd.git"
	indexPath := path.Join("test", "index.html")

	if swarm.GetModeEnabled() {
		payloadFile = githubPayloadFileSwarmMode
		cloneUrl = "https://github.com/kimdre/doco-cd_tests.git"
		indexPath = path.Join("html", "index.html")
	}

	indexPath = path.Join(tmpDir, git.GetRepoName(cloneUrl), indexPath)

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

	h := handlerData{
		dockerCli:  dockerCli,
		appConfig:  appConfig,
		appVersion: app.Version,
		dataMountPoint: container.MountPoint{
			Type:        "bind",
			Source:      tmpDir,
			Destination: tmpDir,
			Mode:        "rw",
		},
		log:      log,
		testName: stackName,
	}

	req := newWebhookRequest(t, webhookPath+"?wait=true", minifiedPayload.Bytes(), appConfig)

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

	if swarm.GetModeEnabled() {
		t.Log("Testing in Swarm mode")

		inspectName := stackName + "_" + "test"

		svc, err := docker.WaitForSwarmService(ctx, t, dockerClient, inspectName, 30*time.Second)
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

	fileContent, err := os.ReadFile(indexPath) // #nosec G304
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

	h := handlerData{
		appConfig:  appConfig,
		appVersion: app.Version,
		dataMountPoint: container.MountPoint{
			Type:        "bind",
			Source:      t.TempDir(),
			Destination: t.TempDir(),
			Mode:        "rw",
		},
		log: log,
	}

	testCases := []struct {
		name string
		url  string
	}{
		{
			name: "Default async when wait not set",
			url:  webhookPath,
		},
		{
			name: "Synchronous when wait=true",
			url:  webhookPath + "?wait=true",
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
