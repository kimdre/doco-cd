package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"regexp"
	"testing"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"

	"github.com/docker/docker/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/webhook"
)

const (
	githubPayloadFile = "testdata/github_payload.json"
)

// Make http call to HealthCheckHandler
func TestHandlerData_HealthCheckHandler(t *testing.T) {
	expectedResponse := fmt.Sprintln(`{"details":"healthy"}`)
	expectedStatusCode := http.StatusOK

	appConfig, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	log := logger.New(12)

	dockerCli, err := docker.CreateDockerCli(appConfig.DockerQuietDeploy, !appConfig.SkipTLSVerification)
	if err != nil {
		t.Fatalf("Failed to create docker client: %v", err)
	}

	t.Cleanup(func() {
		err = dockerCli.Client().Close()
		if err != nil {
			return
		}
	})

	h := handlerData{
		dockerCli: dockerCli,
		appConfig: appConfig,
		log:       log,
	}

	req, err := http.NewRequest("GET", healthPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(h.HealthCheckHandler)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != expectedStatusCode {
		t.Errorf("handler returned wrong status code: got %v want %v", status, expectedStatusCode)
	}

	if rr.Body.String() != expectedResponse {
		t.Errorf("handler returned unexpected body: got %v want %v", rr.Body.String(), expectedResponse)
	}
}

func TestHandlerData_WebhookHandler(t *testing.T) {
	expectedResponse := `{"details":"job completed successfully","job_id":"[a-f0-9-]{36}"}`
	expectedStatusCode := http.StatusCreated
	tmpDir := t.TempDir()

	repoDir := path.Join(tmpDir, "kimdre", "doco-cd")

	payload, err := os.ReadFile(githubPayloadFile)
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

	t.Cleanup(func() {
		err = dockerCli.Client().Close()
		if err != nil {
			return
		}
	})

	h := handlerData{
		dockerCli: dockerCli,
		appConfig: appConfig,
		dataMountPoint: container.MountPoint{
			Type:        "bind",
			Source:      tmpDir,
			Destination: tmpDir,
			Mode:        "rw",
		},
		log: log,
	}

	req, err := http.NewRequest("POST", webhookPath, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set(webhook.GithubSignatureHeader, "sha256="+webhook.GenerateHMAC(payload, appConfig.WebhookSecret))

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
		t.Errorf("handler returned unexpected body: got %v want %v", rr.Body.String(), expectedResponse)
	}

	ctx := context.Background()

	service := compose.NewComposeService(dockerCli)

	downOpts := api.DownOptions{
		RemoveOrphans: true,
		Images:        "all",
		Volumes:       true,
	}

	t.Cleanup(func() {
		t.Log("Remove doco-cd container")

		err = service.Down(ctx, "compose-webhook", downOpts)
		if err != nil {
			t.Fatal(err)
		}
	})

	// Check if the deployed test container is running
	testContainerID, err := docker.GetContainerID(dockerCli.Client(), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		t.Log("Remove test container")

		err = service.Down(ctx, "test", downOpts)
		if err != nil {
			t.Fatal(err)
		}
	})

	testContainer, err := dockerCli.Client().ContainerInspect(ctx, testContainerID)
	if err != nil {
		t.Fatal(err)
	}

	if testContainer.State.Running != true {
		t.Errorf("Test container is not running: %v", testContainer.State)
	}

	// Check if test container returns the expected response on its published port
	testContainerPort := testContainer.NetworkSettings.Ports["80/tcp"][0].HostPort
	testURL := "http://localhost:" + testContainerPort

	resp, err := http.Get(testURL)
	if err != nil {
		t.Fatalf("Failed to make GET request to test container: %v", err)
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

	indexFile := path.Join(repoDir, "test", "index.html")

	fileContent, err := os.ReadFile(indexFile)
	if err != nil {
		t.Fatalf("Failed to read index.html file: %v", err)
	}

	if bodyString != string(fileContent) {
		t.Fatalf("Test container returned unexpected body: got '%v' but want '%v'", bodyString, string(fileContent))
	}
}
