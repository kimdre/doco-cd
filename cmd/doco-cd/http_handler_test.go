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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	testCompose "github.com/testcontainers/testcontainers-go/modules/compose"
	"github.com/testcontainers/testcontainers-go/wait"

	apiInternal "github.com/kimdre/doco-cd/internal/api"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
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

// Make http call to HealthCheckHandler.
func TestHandlerData_HealthCheckHandler(t *testing.T) {
	expectedResponse := `{"content":"healthy","job_id":"[a-f0-9-]{36}"}`
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
		dockerCli:  dockerCli,
		appConfig:  appConfig,
		appVersion: Version,
		log:        log,
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

	regex, err := regexp.Compile(expectedResponse)
	if err != nil {
		t.Fatal(err)
	}

	if !regex.MatchString(rr.Body.String()) {
		t.Errorf("handler returned unexpected body: got %v want %v", rr.Body.String(), expectedResponse)
	}
}

func TestHandlerData_WebhookHandler(t *testing.T) {
	expectedResponse := `{"content":"job completed successfully","job_id":"[a-f0-9-]{36}"}`
	expectedStatusCode := http.StatusCreated
	tmpDir := t.TempDir()

	payloadFile := githubPayloadFile
	cloneUrl := "https://github.com/kimdre/doco-cd.git"
	indexPath := path.Join("test", "index.html")

	if docker.SwarmModeEnabled {
		payloadFile = githubPayloadFileSwarmMode
		cloneUrl = "https://github.com/kimdre/doco-cd_tests.git"
		indexPath = path.Join("html", "index.html")
	}

	indexPath = path.Join(tmpDir, getRepoName(cloneUrl), indexPath)

	payload, err := os.ReadFile(payloadFile)
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
		appVersion:   Version,
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

		err = service.Down(ctx, "test-deploy", downOpts)
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

	testContainerPort := ""

	if docker.SwarmModeEnabled {
		t.Log("Testing in Swarm mode, using service inspect")

		svc, _, err := dockerCli.Client().ServiceInspectWithRaw(ctx, "test-deploy_test", swarm.ServiceInspectOptions{
			InsertDefaults: true,
		})
		if err != nil {
			t.Fatalf("Failed to inspect test container: %v", err)
		}

		if len(svc.Endpoint.Ports) == 0 {
			t.Fatal("Test container has no published ports")
		}

		testContainerPort = strconv.FormatUint(uint64(svc.Endpoint.Ports[0].PublishedPort), 10)

		defer func() {
			err = dockerCli.Client().ServiceRemove(ctx, "test-deploy_test")
			if err != nil {
				t.Fatalf("Failed to remove test container service: %v", err)
			}
		}()
	} else {
		testContainer, err := dockerCli.Client().ContainerInspect(ctx, testContainerID)
		if err != nil {
			t.Fatal(err)
		}

		if testContainer.State.Running != true {
			t.Errorf("Test container is not running: %v", testContainer.State)
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

func TestHandlerData_ProjectApiHandler(t *testing.T) {
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

	tmpDir := t.TempDir()

	h := handlerData{
		dockerCli:    dockerCli,
		dockerClient: dockerClient,
		appConfig:    appConfig,
		appVersion:   Version,
		dataMountPoint: container.MountPoint{
			Type:        "bind",
			Source:      tmpDir,
			Destination: tmpDir,
			Mode:        "rw",
		},
		log: log,
	}

	testCases := []struct {
		name           string
		pattern        string
		path           string
		method         string
		handler        http.HandlerFunc
		expectedStatus int
	}{
		{"Get all Projects", "/projects", "/projects?all=true", http.MethodGet, h.GetProjectsApiHandler, http.StatusOK},
		{"Get Project", "/project/{projectName}", "/project/test", http.MethodGet, h.ProjectApiHandler, http.StatusOK},
		{"Get Project - Non-existent Project", "/project/{projectName}", "/project/nonexistent", http.MethodGet, h.ProjectApiHandler, http.StatusNotFound},
		{"Get Project - Missing Path Param", "/project/{projectName}", "/project/", http.MethodGet, h.ProjectApiHandler, http.StatusNotFound},
		{"Remove Project - With all volumes", "/project/{projectName}", "/project/test?volumes=true&images=false", http.MethodDelete, h.ProjectApiHandler, http.StatusOK},
		{"Remove Project - With all images", "/project/{projectName}", "/project/test?volumes=false&images=true", http.MethodDelete, h.ProjectApiHandler, http.StatusOK},
		{"Remove Project - Invalid images Param", "/project/{projectName}", "/project/test?images=x", http.MethodDelete, h.ProjectApiHandler, http.StatusBadRequest},
		{"Remove Project - Invalid volumes Param", "/project/{projectName}", "/project/test?volumes=x", http.MethodDelete, h.ProjectApiHandler, http.StatusBadRequest},
		{"Restart Project", "/project/{projectName}/{action}", "/project/test/restart", http.MethodPost, h.ProjectActionApiHandler, http.StatusOK},
		{"Restart Project - Non-existent Project", "/project/{projectName}/{action}", "/project/nonexistent/restart", http.MethodPost, h.ProjectActionApiHandler, http.StatusNotFound},
		{"Restart Project - With Timeout", "/project/{projectName}/{action}", "/project/test/restart?timeout=60", http.MethodPost, h.ProjectActionApiHandler, http.StatusOK},
		{"Restart Project - Invalid Timeout", "/project/{projectName}/{action}", "/project/test/restart?timeout=x", http.MethodPost, h.ProjectActionApiHandler, http.StatusBadRequest},
		{"Restart Project - Invalid Method", "/project/{projectName}/{action}", "/project/test/restart", http.MethodGet, h.ProjectActionApiHandler, http.StatusMethodNotAllowed},
		{"Stop Project", "/project/{projectName}/{action}", "/project/test/stop", http.MethodPost, h.ProjectActionApiHandler, http.StatusOK},
		{"Stop Project - Non-existent Project", "/project/{projectName}/{action}", "/project/nonexistent/stop", http.MethodPost, h.ProjectActionApiHandler, http.StatusNotFound},
		{"Start Project", "/project/{projectName}/{action}", "/project/test/start", http.MethodPost, h.ProjectActionApiHandler, http.StatusOK},
		{"Invalid Action", "/project/{projectName}/{action}", "/project/test/invalid", http.MethodPost, h.ProjectActionApiHandler, http.StatusBadRequest},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if docker.SwarmModeEnabled {
				t.Skip("Skipping Project API tests in Swarm mode")
			}

			t.Chdir(tmpDir)

			ctx := context.Background()

			var stack *testCompose.DockerCompose

			stack, err = testCompose.NewDockerComposeWith(
				testCompose.StackIdentifier("test"),
				testCompose.WithStackReaders(strings.NewReader(composeContent)),
			)
			if err != nil {
				t.Fatalf("failed to create stack: %v", err)
			}

			err = stack.
				WaitForService("nginx", wait.ForListeningPort("80/tcp")).
				Up(ctx, testCompose.Wait(true))
			if err != nil {
				t.Fatalf("failed to start stack: %v", err)
			}

			t.Cleanup(func() {
				err = stack.Down(
					context.Background(),
					testCompose.RemoveOrphans(true),
					testCompose.RemoveVolumes(true),
					testCompose.RemoveImagesLocal,
				)
				if err != nil {
					t.Fatalf("Failed to stop stack: %v", err)
				}
			})

			endpointPath := path.Join(apiPath, tc.path)
			endpointPattern := path.Join(apiPath, tc.pattern)

			t.Logf("Testing API endpoint: %s", endpointPath)

			rr := httptest.NewRecorder()
			mux := http.NewServeMux()
			mux.HandleFunc(endpointPattern, tc.handler)

			req, err := http.NewRequest(tc.method, endpointPath, nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set(apiInternal.KeyHeader, appConfig.ApiSecret)
			mux.ServeHTTP(rr, req)

			t.Logf("API response: %s", rr.Body.String())

			if status := rr.Code; status != tc.expectedStatus {
				t.Errorf("handler returned wrong status code: got %v want %v", status, tc.expectedStatus)
			}
		})
	}
}
