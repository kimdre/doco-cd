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

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/docker/swarm"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/webhook"
)

const (
	githubPayloadFile          = "testdata/github_payload.json"
	githubPayloadFileSwarmMode = "testdata/github_payload_swarm_mode.json"
	composeContent             = `services:
  nginx:
    image: nginx:latest
    ports:
      - "80:80"
`
)

func TestHandlerData_WebhookHandler(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	expectedResponse := `{"content":"job completed successfully","job_id":"[a-f0-9-]{36}"}`
	expectedStatusCode := http.StatusCreated
	tmpDir := t.TempDir()

	const containerName = "test"

	stackName := test.ConvertTestName(t.Name())

	payloadFile := githubPayloadFile
	cloneUrl := "https://github.com/kimdre/doco-cd.git"
	indexPath := path.Join("test", "index.html")

	if swarm.ModeEnabled {
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

	appConfig, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	log := logger.New(12)

	dockerCli, err := docker.CreateDockerCli(appConfig.DockerQuietDeploy, !appConfig.SkipTLSVerification)
	if err != nil {
		t.Fatalf("Failed to create docker client: %v", err)
	}

	dockerClient, _ := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)

	t.Cleanup(func() {
		err = dockerCli.Client().Close()
		if err != nil {
			return
		}
	})

	h := handlerData{
		dockerCli:    dockerCli,
		dockerClient: dockerClient,
		appConfig:    appConfig,
		appVersion:   config.AppVersion,
		dataMountPoint: container.MountPoint{
			Type:        "bind",
			Source:      tmpDir,
			Destination: tmpDir,
			Mode:        "rw",
		},
		log:      log,
		testName: stackName,
	}

	req, err := http.NewRequest("POST", webhookPath+"?wait=true", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set(webhook.ScmProviderSecurityHeaders[webhook.Github], "sha256="+webhook.GenerateHMAC(payload, appConfig.WebhookSecret))
	req.Header.Set(webhook.ScmProviderEventHeaders[webhook.Github], "push")

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(h.WebhookHandler)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != expectedStatusCode {
		t.Errorf("handler returned wrong status code: got %v want %v", status, expectedStatusCode)
	}

	regex, err := regexp.Compile(expectedResponse)
	if err != nil {
		t.Fatal(err)
	}

	if !regex.MatchString(rr.Body.String()) {
		t.Fatalf("handler returned unexpected body: got %v want %v", rr.Body.String(), expectedResponse)
	}

	ctx := context.Background()

	service := compose.NewComposeService(dockerCli)

	downOpts := api.DownOptions{
		RemoveOrphans: true,
		Images:        "all",
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

	// Check if the deployed test container is running
	testContainerID, err := docker.GetContainerID(dockerCli.Client(), containerName)
	if err != nil {
		t.Fatal(err)
	}

	testContainerPort := ""

	if swarm.ModeEnabled {
		t.Log("Testing in Swarm mode, using service inspect")

		inspectName := stackName + "_" + containerName

		svc, err := docker.WaitForSwarmService(ctx, t, dockerClient, inspectName, 30*time.Second)
		if err != nil {
			t.Fatalf("Failed to find swarm service for test container: %v", err)
		}

		if len(svc.Endpoint.Ports) == 0 {
			t.Fatal("Test service has no published ports")
		}

		testContainerPort = strconv.FormatUint(uint64(svc.Endpoint.Ports[0].PublishedPort), 10)

		t.Cleanup(func() {
			err = dockerCli.Client().ServiceRemove(ctx, inspectName)
			if err != nil {
				t.Fatalf("Failed to remove test container service: %v", err)
			}
		})
	} else {
		testContainer, err := dockerCli.Client().ContainerInspect(ctx, testContainerID)
		if err != nil {
			t.Fatal(err)
		}

		if testContainer.State.Running != true {
			t.Fatal("Test container is not running")
		}

		// Check if test container returns the expected response on its published port
		networkPort := testContainer.NetworkSettings.Ports["80/tcp"]

		testContainerPort = networkPort[0].HostPort
	}

	testURL := "http://127.0.0.1:" + testContainerPort
	t.Logf("Test URL: %s", testURL)

	httpClient := &http.Client{Timeout: 3 * time.Second}

	resp := &http.Response{}
	for i := 0; i < 10; i++ {
		resp, err = httpClient.Get(testURL) // #nosec G107
		if err != nil {
			t.Logf("Failed to make GET request to test container (attempt %d): %v", i+1, err)
			time.Sleep(3 * time.Second) // Wait before retrying

			continue
		}

		if resp.StatusCode == http.StatusOK {
			t.Logf("Successfully connected to test container on attempt %d", i+1)
			break
		}

		t.Logf("Test container returned status code %d on attempt %d", resp.StatusCode, i+1)

		err = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}

		time.Sleep(3 * time.Second) // Wait before retrying
	}

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

func TestWebhookHandler_WaitQueryParam(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	appConfig, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	log := logger.New(12)

	h := handlerData{
		appConfig:  appConfig,
		appVersion: config.AppVersion,
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
