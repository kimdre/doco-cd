package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/controlplane"

	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
)

const projectAPIComposeContent = `services:
  nginx:
    image: nginx:latest
    ports:
      - "80"
`

func TestHandler_ProjectApiHandler(t *testing.T) {
	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.APIKeys = testAPIKeys("test-api-secret")

	log := logger.New(logger.LevelCritical)

	dockerCli, err := docker.CreateDockerCli(appConfig.DockerQuietDeploy)
	if err != nil {
		t.Fatalf("Failed to create docker client: %v", err)
	}

	t.Cleanup(func() {
		err = dockerCli.Client().Close()
		if err != nil {
			return
		}
	})

	h := Handler{
		dockerCli: dockerCli,
		appConfig: appConfig,
		log:       log,
	}

	testCases := []struct {
		name           string
		path           string
		method         string
		expectedStatus int
		assertRunning  bool
	}{
		{"Get all Projects", "/projects?all=true", http.MethodGet, http.StatusOK, false},
		{"Get Project", "/project/{projectName}", http.MethodGet, http.StatusOK, false},
		{"Get Project - Non-existent Project", "/project/nonexistent", http.MethodGet, http.StatusNotFound, false},
		{"Get Project - Missing Path Param", "/project/", http.MethodGet, http.StatusNotFound, false},
		{"Remove Project - With all volumes", "/project/{projectName}?volumes=true&images=false", http.MethodDelete, http.StatusOK, false},
		{"Remove Project - With all images", "/project/{projectName}?volumes=false&images=true", http.MethodDelete, http.StatusOK, false},
		{"Remove Project - Invalid images Param", "/project/{projectName}?images=x", http.MethodDelete, http.StatusBadRequest, false},
		{"Remove Project - Invalid volumes Param", "/project/{projectName}?volumes=x", http.MethodDelete, http.StatusBadRequest, false},
		{"Restart Project", "/project/{projectName}/restart", http.MethodPost, http.StatusOK, false},
		{"Restart Project - Non-existent Project", "/project/nonexistent/restart", http.MethodPost, http.StatusNotFound, false},
		{"Restart Project - Non-existent Project and Zero Timeout", "/project/nonexistent/restart?timeout=0", http.MethodPost, http.StatusNotFound, false},
		{"Restart Project - Non-existent Project and Overflowing Timeout", "/project/nonexistent/restart?timeout=" + strconv.FormatInt(controlplane.MaxProjectActionTimeout+1, 10), http.MethodPost, http.StatusNotFound, false},
		{"Restart Project - With Timeout", "/project/{projectName}/restart?timeout=60", http.MethodPost, http.StatusOK, false},
		{"Restart Project - Invalid Timeout", "/project/{projectName}/restart?timeout=x", http.MethodPost, http.StatusBadRequest, false},
		{"Recreate Project", "/project/{projectName}/recreate", http.MethodPost, http.StatusOK, false},
		{"Recreate Project Service", "/project/{projectName}/recreate?service=nginx", http.MethodPost, http.StatusOK, false},
		{"Recreate Project Missing Service", "/project/{projectName}/recreate?service=missing", http.MethodPost, http.StatusNotFound, false},
		{"Stop Project - Invalid Timeout", "/project/{projectName}/stop?timeout=x", http.MethodPost, http.StatusBadRequest, true},
		{"Stop Project - Zero Timeout", "/project/{projectName}/stop?timeout=0", http.MethodPost, http.StatusBadRequest, true},
		{"Stop Project - Negative Timeout", "/project/{projectName}/stop?timeout=-1", http.MethodPost, http.StatusBadRequest, true},
		{"Stop Project - Overflowing Timeout", "/project/{projectName}/stop?timeout=" + strconv.FormatInt(controlplane.MaxProjectActionTimeout+1, 10), http.MethodPost, http.StatusBadRequest, true},
		{"Restart Project - Invalid Method", "/project/{projectName}/restart", http.MethodGet, http.StatusMethodNotAllowed, false},
		{"Restart Project - Invalid Method and Non-existent Project", "/project/nonexistent/restart", http.MethodGet, http.StatusNotFound, false},
		{"Stop Project", "/project/{projectName}/stop", http.MethodPost, http.StatusOK, false},
		{"Stop Project - Invalid Method", "/project/{projectName}/stop", http.MethodGet, http.StatusMethodNotAllowed, true},
		{"Stop Project - Invalid Method and Zero Timeout", "/project/{projectName}/stop?timeout=0", http.MethodGet, http.StatusMethodNotAllowed, true},
		{"Stop Project - Non-existent Project", "/project/nonexistent/stop", http.MethodPost, http.StatusNotFound, false},
		{"Start Project", "/project/{projectName}/start", http.MethodPost, http.StatusOK, false},
		{"Invalid Action", "/project/{projectName}/invalid", http.MethodPost, http.StatusNotFound, false},
		{"Invalid Action - Invalid Method", "/project/{projectName}/invalid", http.MethodGet, http.StatusNotFound, false},
		{"Invalid Action - Non-existent Project", "/project/nonexistent/invalid", http.MethodPost, http.StatusNotFound, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if swarmModeEnabled {
				t.Skip("Skipping Project API tests in Swarm mode")
			}

			ctx := context.Background()

			stackName := test.ConvertTestName(t.Name())

			test.ComposeUp(ctx, t, test.WithYAML(projectAPIComposeContent), test.WithName(stackName))

			endpointPath := path.Join(APIPath, strings.Replace(tc.path, "{projectName}", stackName, 1))
			t.Logf("Testing API endpoint: %s", endpointPath)

			rr := httptest.NewRecorder()

			mux := http.NewServeMux()
			if _, err := RegisterRoutes(mux, &h, Mounts{}); err != nil {
				t.Fatal(err)
			}

			req, err := http.NewRequest(tc.method, endpointPath, nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set(restAPI.KeyHeader, appConfig.APIKeys[0].Key)
			mux.ServeHTTP(rr, req)

			t.Logf("API response: %s", rr.Body.String())

			if status := rr.Code; status != tc.expectedStatus {
				t.Errorf("handler returned wrong status code: got %v want %v", status, tc.expectedStatus)
			}

			if tc.assertRunning {
				containers, err := docker.GetProjectContainers(ctx, dockerCli, stackName)
				if err != nil {
					t.Fatal(err)
				}

				if len(containers) == 0 || !strings.EqualFold(string(containers[0].State), "running") {
					t.Fatalf("invalid method must not stop project containers: %#v", containers)
				}
			}

			if strings.HasPrefix(tc.name, "Remove Project - Invalid") {
				containers, err := docker.GetProjectContainers(ctx, dockerCli, stackName)
				if err != nil {
					t.Fatal(err)
				}

				if len(containers) == 0 {
					t.Fatal("invalid remove parameter must not remove project containers")
				}
			}
		})
	}
}

// TestGetProjectsApiHandlerInvalidAllParamSingleResponse pins the
// double-response regression: an invalid ?all= value must produce exactly one
// 400 JSON document and stop before listing projects on the Docker daemon.
func TestGetProjectsApiHandlerInvalidAllParamSingleResponse(t *testing.T) {
	// Only project/container listing counts as reaching the daemon; the Docker
	// client may issue bookkeeping requests such as /_ping on its own.
	var daemonListRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/containers") {
			daemonListRequests.Add(1)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.52")

	dockerCli, err := docker.CreateDockerCli(true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	h := &Handler{
		dockerCli: dockerCli,
		appConfig: &app.Config{APIKeys: testAPIKeys("projects-test-secret")}, // #nosec G101 -- test fixture, not a real credential.
		log:       logger.New(logger.LevelCritical),
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects?all=banana", nil)
	req.Header.Set(restAPI.KeyHeader, h.appConfig.APIKeys[0].Key)

	h.GetProjectsApiHandler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}

	decoder := json.NewDecoder(strings.NewReader(rr.Body.String()))

	var response restAPI.ErrorResponse
	if err := decoder.Decode(&response); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if decoder.More() {
		t.Fatalf("expected exactly one JSON document in response, got %q", rr.Body.String())
	}

	if content, ok := response.Content.(string); !ok || !strings.Contains(content, "must be true or false") {
		t.Fatalf("unexpected error content: %#v", response)
	}

	if got := daemonListRequests.Load(); got != 0 {
		t.Fatalf("invalid parameter reached the Docker daemon listing %d time(s)", got)
	}
}
