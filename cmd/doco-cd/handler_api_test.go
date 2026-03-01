package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	testCompose "github.com/testcontainers/testcontainers-go/modules/compose"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
)

// Make http call to HealthCheckHandler.
func TestHandlerData_HealthCheckHandler(t *testing.T) {
	t.Parallel()

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
		appVersion: config.AppVersion,
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
		appVersion:   config.AppVersion,
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
		{"Get Project", "/project/{projectName}", "/project/{projectName}", http.MethodGet, h.ProjectApiHandler, http.StatusOK},
		{"Get Project - Non-existent Project", "/project/{projectName}", "/project/nonexistent", http.MethodGet, h.ProjectApiHandler, http.StatusNotFound},
		{"Get Project - Missing Path Param", "/project/{projectName}", "/project/", http.MethodGet, h.ProjectApiHandler, http.StatusNotFound},
		{"Remove Project - With all volumes", "/project/{projectName}", "/project/{projectName}?volumes=true&images=false", http.MethodDelete, h.ProjectApiHandler, http.StatusOK},
		{"Remove Project - With all images", "/project/{projectName}", "/project/{projectName}?volumes=false&images=true", http.MethodDelete, h.ProjectApiHandler, http.StatusOK},
		{"Remove Project - Invalid images Param", "/project/{projectName}", "/project/{projectName}?images=x", http.MethodDelete, h.ProjectApiHandler, http.StatusBadRequest},
		{"Remove Project - Invalid volumes Param", "/project/{projectName}", "/project/{projectName}?volumes=x", http.MethodDelete, h.ProjectApiHandler, http.StatusBadRequest},
		{"Restart Project", "/project/{projectName}/{action}", "/project/{projectName}/restart", http.MethodPost, h.ProjectActionApiHandler, http.StatusOK},
		{"Restart Project - Non-existent Project", "/project/{projectName}/{action}", "/project/nonexistent/restart", http.MethodPost, h.ProjectActionApiHandler, http.StatusNotFound},
		{"Restart Project - With Timeout", "/project/{projectName}/{action}", "/project/{projectName}/restart?timeout=60", http.MethodPost, h.ProjectActionApiHandler, http.StatusOK},
		{"Restart Project - Invalid Timeout", "/project/{projectName}/{action}", "/project/{projectName}/restart?timeout=x", http.MethodPost, h.ProjectActionApiHandler, http.StatusBadRequest},
		{"Restart Project - Invalid Method", "/project/{projectName}/{action}", "/project/{projectName}/restart", http.MethodGet, h.ProjectActionApiHandler, http.StatusMethodNotAllowed},
		{"Stop Project", "/project/{projectName}/{action}", "/project/{projectName}/stop", http.MethodPost, h.ProjectActionApiHandler, http.StatusOK},
		{"Stop Project - Non-existent Project", "/project/{projectName}/{action}", "/project/nonexistent/stop", http.MethodPost, h.ProjectActionApiHandler, http.StatusNotFound},
		{"Start Project", "/project/{projectName}/{action}", "/project/{projectName}/start", http.MethodPost, h.ProjectActionApiHandler, http.StatusOK},
		{"Invalid Action", "/project/{projectName}/{action}", "/project/{projectName}/invalid", http.MethodPost, h.ProjectActionApiHandler, http.StatusBadRequest},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if swarm.ModeEnabled {
				t.Skip("Skipping Project API tests in Swarm mode")
			}

			ctx := context.Background()

			var stack *testCompose.DockerCompose

			stackName := test.ConvertTestName(t.Name())

			stack, err = testCompose.NewDockerComposeWith(
				testCompose.StackIdentifier(stackName),
				testCompose.WithStackReaders(strings.NewReader(composeContent)),
			)
			if err != nil {
				t.Fatalf("failed to create stack: %v", err)
			}

			// Retry starting the stack up to 3 times
			const maxRetries = 3
			for i := 0; i < maxRetries; i++ {
				err = stack.
					WaitForService("nginx", wait.ForListeningPort("80/tcp")).
					Up(ctx, testCompose.Wait(true))
				if err == nil {
					break
				}

				if i < maxRetries-1 {
					t.Logf("failed to start stack (attempt %d/%d): %v, retrying...", i+1, maxRetries, err)
				}
			}

			if err != nil {
				t.Fatalf("failed to start stack after %d attempts: %v", maxRetries, err)
			}

			t.Cleanup(func() {
				err = stack.Down(
					context.Background(),
					testCompose.RemoveOrphans(true),
					testCompose.RemoveVolumes(true),
				)
				if err != nil {
					t.Fatalf("Failed to stop stack: %v", err)
				}
			})

			endpointPath := path.Join(apiPath, strings.Replace(tc.path, "{projectName}", stackName, 1))
			endpointPattern := path.Join(apiPath, tc.pattern)

			t.Logf("Testing API endpoint: %s", endpointPath)

			rr := httptest.NewRecorder()
			mux := http.NewServeMux()
			mux.HandleFunc(endpointPattern, tc.handler)

			req, err := http.NewRequest(tc.method, endpointPath, nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)
			mux.ServeHTTP(rr, req)

			t.Logf("API response: %s", rr.Body.String())

			if status := rr.Code; status != tc.expectedStatus {
				t.Errorf("handler returned wrong status code: got %v want %v", status, tc.expectedStatus)
			}
		})
	}
}
