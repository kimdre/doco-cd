package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/container"
	dockerswarmtypes "github.com/moby/moby/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"

	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

func TestDockerCliForRequestContextValidation(t *testing.T) {
	t.Parallel()

	h := handlerData{log: logger.New(logger.LevelCritical)}
	jobLog := h.log.With()

	tests := []struct {
		name       string
		url        string
		wantOK     bool
		wantStatus int
		wantHeader string
	}{
		{"default omitted", "/v1/api/projects", true, http.StatusOK, "default"},
		{"default explicit", "/v1/api/projects?context=default", true, http.StatusOK, "default"},
		{"unknown named context", "/v1/api/projects?context=remote", false, http.StatusBadRequest, "remote"},
		{"repeated context", "/v1/api/projects?context=default&context=remote", false, http.StatusBadRequest, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			recorder := httptest.NewRecorder()

			_, _, ok := h.dockerCliForRequest(recorder, req, jobLog, "job-id")
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}

			status := recorder.Code
			if tt.wantOK {
				status = http.StatusOK
			}

			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}

			if got := recorder.Header().Get(dockerContextHeader); got != tt.wantHeader {
				t.Fatalf("%s = %q, want %q", dockerContextHeader, got, tt.wantHeader)
			}
		})
	}
}

// Make http call to HealthCheckHandler.
func TestHandlerData_HealthCheckHandler(t *testing.T) {
	t.Parallel()

	expectedResponse := `{"content":"healthy","job_id":"[a-f0-9-]{36}"}`
	expectedStatusCode := http.StatusOK

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
	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

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

	tmpDir := t.TempDir()

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
		log: log,
	}

	testCases := []struct {
		name           string
		pattern        string
		path           string
		method         string
		handler        http.HandlerFunc
		expectedStatus int
		assertRunning  bool
	}{
		{"Get all Projects", "/projects", "/projects?all=true", http.MethodGet, h.GetProjectsApiHandler, http.StatusOK, false},
		{"Get Project", "/project/{projectName}", "/project/{projectName}", http.MethodGet, h.ProjectApiHandler, http.StatusOK, false},
		{"Get Project - Non-existent Project", "/project/{projectName}", "/project/nonexistent", http.MethodGet, h.ProjectApiHandler, http.StatusNotFound, false},
		{"Get Project - Missing Path Param", "/project/{projectName}", "/project/", http.MethodGet, h.ProjectApiHandler, http.StatusNotFound, false},
		{"Remove Project - With all volumes", "/project/{projectName}", "/project/{projectName}?volumes=true&images=false", http.MethodDelete, h.ProjectApiHandler, http.StatusOK, false},
		{"Remove Project - With all images", "/project/{projectName}", "/project/{projectName}?volumes=false&images=true", http.MethodDelete, h.ProjectApiHandler, http.StatusOK, false},
		{"Remove Project - Invalid images Param", "/project/{projectName}", "/project/{projectName}?images=x", http.MethodDelete, h.ProjectApiHandler, http.StatusBadRequest, false},
		{"Remove Project - Invalid volumes Param", "/project/{projectName}", "/project/{projectName}?volumes=x", http.MethodDelete, h.ProjectApiHandler, http.StatusBadRequest, false},
		{"Restart Project", "/project/{projectName}/{action}", "/project/{projectName}/restart", http.MethodPost, h.ProjectActionApiHandler, http.StatusOK, false},
		{"Restart Project - Non-existent Project", "/project/{projectName}/{action}", "/project/nonexistent/restart", http.MethodPost, h.ProjectActionApiHandler, http.StatusNotFound, false},
		{"Restart Project - Non-existent Project and Zero Timeout", "/project/{projectName}/{action}", "/project/nonexistent/restart?timeout=0", http.MethodPost, h.ProjectActionApiHandler, http.StatusNotFound, false},
		{"Restart Project - Non-existent Project and Overflowing Timeout", "/project/{projectName}/{action}", "/project/nonexistent/restart?timeout=" + strconv.FormatInt(maxProjectActionTimeout+1, 10), http.MethodPost, h.ProjectActionApiHandler, http.StatusNotFound, false},
		{"Restart Project - With Timeout", "/project/{projectName}/{action}", "/project/{projectName}/restart?timeout=60", http.MethodPost, h.ProjectActionApiHandler, http.StatusOK, false},
		{"Restart Project - Invalid Timeout", "/project/{projectName}/{action}", "/project/{projectName}/restart?timeout=x", http.MethodPost, h.ProjectActionApiHandler, http.StatusBadRequest, false},
		{"Stop Project - Invalid Timeout", "/project/{projectName}/{action}", "/project/{projectName}/stop?timeout=x", http.MethodPost, h.ProjectActionApiHandler, http.StatusBadRequest, true},
		{"Stop Project - Zero Timeout", "/project/{projectName}/{action}", "/project/{projectName}/stop?timeout=0", http.MethodPost, h.ProjectActionApiHandler, http.StatusBadRequest, true},
		{"Stop Project - Negative Timeout", "/project/{projectName}/{action}", "/project/{projectName}/stop?timeout=-1", http.MethodPost, h.ProjectActionApiHandler, http.StatusBadRequest, true},
		{"Stop Project - Overflowing Timeout", "/project/{projectName}/{action}", "/project/{projectName}/stop?timeout=" + strconv.FormatInt(maxProjectActionTimeout+1, 10), http.MethodPost, h.ProjectActionApiHandler, http.StatusBadRequest, true},
		{"Restart Project - Invalid Method", "/project/{projectName}/{action}", "/project/{projectName}/restart", http.MethodGet, h.ProjectActionApiHandler, http.StatusMethodNotAllowed, false},
		{"Restart Project - Invalid Method and Non-existent Project", "/project/{projectName}/{action}", "/project/nonexistent/restart", http.MethodGet, h.ProjectActionApiHandler, http.StatusNotFound, false},
		{"Stop Project", "/project/{projectName}/{action}", "/project/{projectName}/stop", http.MethodPost, h.ProjectActionApiHandler, http.StatusOK, false},
		{"Stop Project - Invalid Method", "/project/{projectName}/{action}", "/project/{projectName}/stop", http.MethodGet, h.ProjectActionApiHandler, http.StatusMethodNotAllowed, true},
		{"Stop Project - Invalid Method and Zero Timeout", "/project/{projectName}/{action}", "/project/{projectName}/stop?timeout=0", http.MethodGet, h.ProjectActionApiHandler, http.StatusMethodNotAllowed, true},
		{"Stop Project - Invalid Method and Negative Timeout", "/project/{projectName}/{action}", "/project/{projectName}/stop?timeout=-1", http.MethodGet, h.ProjectActionApiHandler, http.StatusMethodNotAllowed, true},
		{"Stop Project - Invalid Method and Overflowing Timeout", "/project/{projectName}/{action}", "/project/{projectName}/stop?timeout=" + strconv.FormatInt(maxProjectActionTimeout+1, 10), http.MethodGet, h.ProjectActionApiHandler, http.StatusMethodNotAllowed, true},
		{"Stop Project - Non-existent Project", "/project/{projectName}/{action}", "/project/nonexistent/stop", http.MethodPost, h.ProjectActionApiHandler, http.StatusNotFound, false},
		{"Start Project", "/project/{projectName}/{action}", "/project/{projectName}/start", http.MethodPost, h.ProjectActionApiHandler, http.StatusOK, false},
		{"Invalid Action", "/project/{projectName}/{action}", "/project/{projectName}/invalid", http.MethodPost, h.ProjectActionApiHandler, http.StatusBadRequest, false},
		{"Invalid Action - Invalid Method", "/project/{projectName}/{action}", "/project/{projectName}/invalid", http.MethodGet, h.ProjectActionApiHandler, http.StatusBadRequest, false},
		{"Invalid Action - Non-existent Project", "/project/{projectName}/{action}", "/project/nonexistent/invalid", http.MethodPost, h.ProjectActionApiHandler, http.StatusNotFound, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if swarm.GetModeEnabled() {
				t.Skip("Skipping Project API tests in Swarm mode")
			}

			ctx := context.Background()

			stackName := test.ConvertTestName(t.Name())

			test.ComposeUp(ctx, t, test.WithYAML(composeContent), test.WithName(stackName))

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

func TestStackActionApiHandlerValidationPrecedence(t *testing.T) {
	var actionRequests int

	h := newStackActionRESTTestHandler(t, []dockerswarmtypes.Service{{
		Spec: dockerswarmtypes.ServiceSpec{
			Annotations: dockerswarmtypes.Annotations{Name: "stack_web"},
			Mode:        dockerswarmtypes.ServiceMode{Replicated: &dockerswarmtypes.ReplicatedService{}},
		},
	}}, &actionRequests)

	for _, testCase := range []struct {
		name       string
		path       string
		method     string
		wantStatus int
		contains   string
	}{
		{name: "invalid action before method", path: "/stack/stack/invalid", method: http.MethodGet, wantStatus: http.StatusBadRequest, contains: "invalid action"},
		{name: "method before wait parsing", path: "/stack/stack/restart?wait=invalid", method: http.MethodGet, wantStatus: http.StatusMethodNotAllowed, contains: "invalid http method"},
		{name: "invalid wait aborts action", path: "/stack/stack/restart?wait=invalid", method: http.MethodPost, wantStatus: http.StatusBadRequest, contains: "wait"},
		{name: "invalid replicas aborts action", path: "/stack/stack/scale?replicas=invalid", method: http.MethodPost, wantStatus: http.StatusBadRequest, contains: "replicas"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux := http.NewServeMux()
			mux.HandleFunc("/stack/{stackName}/{action}", h.StackActionApiHandler)

			req := httptest.NewRequest(testCase.method, testCase.path, nil)
			req.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)
			mux.ServeHTTP(rr, req)

			if rr.Code != testCase.wantStatus || !strings.Contains(rr.Body.String(), testCase.contains) {
				t.Fatalf("response = %d %s, want %d containing %q", rr.Code, rr.Body.String(), testCase.wantStatus, testCase.contains)
			}
		})
	}

	if actionRequests != 0 {
		t.Fatalf("validation failures dispatched %d stack actions", actionRequests)
	}
}

func TestStackActionApiHandlerLooksUpStackBeforeValidation(t *testing.T) {
	h := newStackActionRESTTestHandler(t, nil, nil)
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("/stack/{stackName}/{action}", h.StackActionApiHandler)

	req := httptest.NewRequest(http.MethodGet, "/stack/missing/invalid", nil)
	req.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "stack not found: missing") {
		t.Fatalf("response = %d %s, want stack lookup 404", rr.Code, rr.Body.String())
	}
}

func TestStackActionApiHandlerReturnsNotFoundForMissingService(t *testing.T) {
	services := []dockerswarmtypes.Service{{
		Spec: dockerswarmtypes.ServiceSpec{
			Annotations: dockerswarmtypes.Annotations{Name: "stack_web"},
			Mode:        dockerswarmtypes.ServiceMode{Replicated: &dockerswarmtypes.ReplicatedService{}},
		},
	}}
	h := newStackActionRESTTestHandler(t, services, nil)

	for _, testCase := range []struct {
		action string
		query  string
	}{
		{action: "scale", query: "service=missing&replicas=1"},
		{action: "restart", query: "service=missing"},
		{action: "run", query: "service=missing"},
	} {
		t.Run(testCase.action, func(t *testing.T) {
			rr := callStackActionREST(t, h, "/stack/stack/"+testCase.action+"?"+testCase.query)

			if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "service not found: stack_missing") {
				t.Fatalf("response = %d %s, want missing service 404", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestStackActionApiHandlerReturnsNotFoundWhenAllServicesSkipped(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		action   string
		query    string
		service  dockerswarmtypes.Service
		contains string
	}{
		{
			name:   "scale global service",
			action: "scale",
			query:  "replicas=1",
			service: dockerswarmtypes.Service{Spec: dockerswarmtypes.ServiceSpec{
				Annotations: dockerswarmtypes.Annotations{Name: "stack_global"},
				Mode:        dockerswarmtypes.ServiceMode{Global: &dockerswarmtypes.GlobalService{}},
			}},
			contains: "no services found to scale in stack: stack",
		},
		{
			name:   "restart job service",
			action: "restart",
			service: dockerswarmtypes.Service{Spec: dockerswarmtypes.ServiceSpec{
				Annotations: dockerswarmtypes.Annotations{Name: "stack_job"},
				Mode:        dockerswarmtypes.ServiceMode{ReplicatedJob: &dockerswarmtypes.ReplicatedJob{}},
			}},
			contains: "no services found to restart in stack: stack",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newStackActionRESTTestHandler(t, []dockerswarmtypes.Service{testCase.service}, nil)
			rr := callStackActionREST(t, h, "/stack/stack/"+testCase.action+"?"+testCase.query)

			if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), testCase.contains) {
				t.Fatalf("response = %d %s, want all-skipped 404 containing %q", rr.Code, rr.Body.String(), testCase.contains)
			}
		})
	}
}

func callStackActionREST(t *testing.T, h *handlerData, requestPath string) *httptest.ResponseRecorder {
	t.Helper()

	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("/stack/{stackName}/{action}", h.StackActionApiHandler)

	req := httptest.NewRequest(http.MethodPost, requestPath, nil)
	req.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)
	mux.ServeHTTP(rr, req)

	return rr
}

func newStackActionRESTTestHandler(t *testing.T, services []dockerswarmtypes.Service, actionRequests *int) *handlerData {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/services"):
			_ = json.NewEncoder(w).Encode(services)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/services/"):
			if actionRequests != nil {
				(*actionRequests)++
			}

			serviceName := path.Base(r.URL.Path)
			for _, service := range services {
				if service.Spec.Name == serviceName {
					_ = json.NewEncoder(w).Encode(service)

					return
				}
			}

			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/services/"):
			if actionRequests != nil {
				(*actionRequests)++
			}

			http.Error(w, "unexpected action request", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", server.URL)
	t.Setenv("DOCKER_API_VERSION", "1.52")

	dockerCli, err := docker.CreateDockerCli(true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	return &handlerData{
		dockerCli:  dockerCli,
		appConfig:  &app.Config{ApiSecret: "stack-test-secret"}, // #nosec G101 -- test fixture, not a real credential.
		appVersion: app.Version,
		log:        logger.New(logger.LevelCritical),
	}
}

func TestHandlerData_TriggerPollHandler(t *testing.T) {
	// This is a placeholder test to ensure the TriggerPollHandler is registered and responds to requests.
	// Implementing a full test would require setting up a mock PollManager and verifying it receives the trigger call.
	testCases := []struct {
		name             string
		payload          *strings.Reader
		wait             bool
		expectedStatus   int
		expectedResponse string
	}{
		{
			name:             "With wait",
			payload:          strings.NewReader(`[{"url": "https://github.com/kimdre/doco-cd_tests.git", "reference": "main"}]`),
			wait:             true,
			expectedStatus:   http.StatusOK,
			expectedResponse: `{"content":"poll jobs complete","job_id":"[a-f0-9-]{36}"}`,
		},
		{
			name:             "Without wait",
			payload:          strings.NewReader(`[{"url": "https://github.com/kimdre/doco-cd_tests.git", "reference": "main"}]`),
			wait:             false,
			expectedStatus:   http.StatusAccepted,
			expectedResponse: `{"content":"poll jobs started","job_id":"[a-f0-9-]{36}"}`,
		},
		{
			name:             "With deploy config",
			payload:          strings.NewReader(`[{"url": "https://github.com/kimdre/doco-cd_tests.git", "reference": "main", "deployments": [{"name": "with-deploy-config"}]}]`),
			wait:             true,
			expectedStatus:   http.StatusOK,
			expectedResponse: `{"content":"poll jobs complete","job_id":"[a-f0-9-]{36}"}`,
		},
		{
			name:             "Empty body",
			payload:          strings.NewReader(``),
			wait:             false,
			expectedStatus:   http.StatusBadRequest,
			expectedResponse: `{"error":"failed to decode json in body","content":"EOF","job_id":"[a-f0-9-]{36}"}`,
		},
		{
			name:             "Invalid JSON",
			payload:          strings.NewReader(`[{"url": "https://github.com/kimdre/doco-cd_tests.git", "reference": "main"]`),
			wait:             false,
			expectedStatus:   http.StatusBadRequest,
			expectedResponse: `{"error":"failed to decode json in body","content":"invalid character ']' after object key:value pair","job_id":"[a-f0-9-]{36}"}`,
		},
	}

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

	dockerCli, err := docker.CreateDockerCli(appConfig.DockerQuietDeploy)
	if err != nil {
		t.Fatalf("Failed to create docker client: %v", err)
	}

	backend, err := compose.NewComposeService(dockerCli)
	if err != nil {
		t.Fatalf("Failed to create compose service: %v", err)
	}

	t.Cleanup(func() {
		err = dockerCli.Client().Close()
		if err != nil {
			return
		}
	})

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := handlerData{
				dockerCli:  dockerCli,
				appConfig:  appConfig,
				appVersion: app.Version,
				log:        logger.New(logger.LevelCritical),
				testName:   test.ConvertTestName(t.Name()),
			}

			endpoint := path.Join(apiPath, "/poll/run")

			rr := httptest.NewRecorder()

			mux := http.NewServeMux()
			mux.HandleFunc(endpoint, h.TriggerPollHandler)

			reqUrl := endpoint
			if tc.wait {
				reqUrl += "?wait=true"
			} else {
				reqUrl += "?wait=false"
			}

			req, err := http.NewRequest("POST", reqUrl, tc.payload)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() {
				downOpts := api.DownOptions{
					RemoveOrphans: true,
					Images:        "local",
					Volumes:       true,
				}

				err = backend.Down(context.Background(), test.ConvertTestName(t.Name()), downOpts)
				if err != nil {
					t.Fatalf("Failed to remove test stack: %v", err)
				}
			})

			// Set headers
			req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)
			mux.ServeHTTP(rr, req)

			if status := rr.Code; status != tc.expectedStatus {
				t.Errorf("handler returned wrong status code: got %v want %v", status, tc.expectedStatus)
			}

			regex, err := regexp.Compile(tc.expectedResponse)
			if err != nil {
				t.Fatal(err)
			}

			if !regex.MatchString(rr.Body.String()) {
				t.Errorf("handler returned unexpected body: got %v want %v", rr.Body.String(), tc.expectedResponse)
			}
		})
	}
}

func TestHandlerData_TriggerPollHandlerWithoutWait_DetachesRequestContext(t *testing.T) {
	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

	ctxCancelled := make(chan bool, 1)

	h := handlerData{
		appConfig: appConfig,
		log:       logger.New(logger.LevelCritical),
		runPoll: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider,
			_ string,
		) error {
			time.Sleep(50 * time.Millisecond)

			select {
			case <-ctx.Done():
				ctxCancelled <- true
			default:
				ctxCancelled <- false
			}

			return nil
		},
	}

	endpoint := path.Join(apiPath, "/poll/run")

	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc(endpoint, h.TriggerPollHandler)

	req, err := http.NewRequest("POST", endpoint+"?wait=false", strings.NewReader(`[{"url":"https://github.com/kimdre/doco-cd_tests.git","reference":"main"}]`))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)
	mux.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusAccepted {
		t.Fatalf("handler returned wrong status code: got %v want %v", status, http.StatusAccepted)
	}

	regex := regexp.MustCompile(`{"content":"poll jobs started","job_id":"[a-f0-9-]{36}"}`)
	if !regex.MatchString(rr.Body.String()) {
		t.Fatalf("handler returned unexpected body: got %v", rr.Body.String())
	}

	select {
	case cancelled := <-ctxCancelled:
		if cancelled {
			t.Fatal("poll run context was cancelled after async API response")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for poll run")
	}
}

func TestHandlerData_TriggerPollHandlerRejectsInvalidRequestsBeforeTracking(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		query      string
		body       string
		maxPayload int64
		wantStatus int
		wantError  string
	}{
		{name: "malformed wait", query: "?wait=eventually", body: `[{"url":"` + validPollSourceURL + `"}]`, maxPayload: 1024, wantStatus: http.StatusBadRequest},
		{name: "invalid JSON", body: `[{`, maxPayload: 1024, wantStatus: http.StatusBadRequest},
		{name: "invalid config", body: `[{}]`, maxPayload: 1024, wantStatus: http.StatusBadRequest},
		{name: "empty config list", body: `[]`, maxPayload: 1024, wantStatus: http.StatusBadRequest, wantError: "no poll configuration provided in request body"},
		{name: "second JSON value", body: `[{"url":"` + validPollSourceURL + `"}] {}`, maxPayload: 1024, wantStatus: http.StatusBadRequest},
		{name: "trailing non-whitespace", body: `[{"url":"` + validPollSourceURL + `"}] trailing`, maxPayload: 1024, wantStatus: http.StatusBadRequest},
		{name: "oversized body", body: `[{"url":"` + validPollSourceURL + `"}]`, maxPayload: 8, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "oversized valid prefix with trailing whitespace", body: `[{"url":"` + validPollSourceURL + `"}]  `, maxPayload: int64(len(`[{"url":"`+validPollSourceURL+`"}]`) + 1), wantStatus: http.StatusRequestEntityTooLarge},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tracker := newDeploymentRunTracker(nil)
			runs := 0
			h := &handlerData{
				appConfig:  &app.Config{ApiSecret: "poll-secret", MaxPayloadSize: testCase.maxPayload}, // #nosec G101 -- test fixture.
				log:        logger.New(logger.LevelCritical),
				runTracker: tracker,
				runPoll: func(context.Context, poll.Config, *app.Config, container.MountPoint,
					command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider, string,
				) error {
					runs++

					return nil
				},
			}
			body := &trackingReadCloser{Reader: strings.NewReader(testCase.body)}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, apiPath+"/poll/run"+testCase.query, body)
			request.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)

			h.TriggerPollHandler(recorder, request)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, testCase.wantStatus, recorder.Body.String())
			}

			if testCase.wantError != "" {
				var response jsonError
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}

				if response.Error != testCase.wantError {
					t.Fatalf("error = %q, want %q", response.Error, testCase.wantError)
				}
			}

			if !body.closed {
				t.Fatal("request body was not closed")
			}

			if runs != 0 {
				t.Fatalf("invalid request started %d poll runs", runs)
			}

			if got := tracker.List(10, string(deploymentRunTriggerPoll), ""); len(got) != 0 {
				t.Fatalf("invalid request was tracked: %#v", got)
			}
		})
	}
}

func TestHandlerData_TriggerPollHandlerAcceptsTrailingWhitespace(t *testing.T) {
	runs := 0
	h := &handlerData{
		appConfig: &app.Config{ApiSecret: "poll-secret", MaxPayloadSize: 1024}, // #nosec G101 -- test fixture.
		log:       logger.New(logger.LevelCritical),
		runPoll: func(context.Context, poll.Config, *app.Config, container.MountPoint,
			command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider, string,
		) error {
			runs++

			return nil
		},
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, apiPath+"/poll/run", strings.NewReader(`[{"url":"`+validPollSourceURL+`"}] \n\t`))
	request.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)

	h.TriggerPollHandler(response, request)

	if response.Code != http.StatusOK || runs != 1 {
		t.Fatalf("status = %d, runs = %d, body = %s", response.Code, runs, response.Body.String())
	}
}

func TestHandlerData_TriggerPollHandlerReportsPollFailures(t *testing.T) {
	tracker := newDeploymentRunTracker(nil)
	h := &handlerData{
		appConfig:  &app.Config{ApiSecret: "poll-secret", MaxPayloadSize: 1024}, // #nosec G101 -- test fixture.
		log:        logger.New(logger.LevelCritical),
		runTracker: tracker,
		runPoll: func(_ context.Context, cfg poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider, _ string,
		) error {
			if strings.Contains(cfg.SourceUrl, "failed") {
				return errors.New("poll failed")
			}

			return nil
		},
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, apiPath+"/poll/run", strings.NewReader(`[
		{"url":"https://example.com/succeeded.git"},
		{"url":"https://example.com/failed.git"}
	]`))
	request.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)

	h.TriggerPollHandler(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusInternalServerError, response.Body.String())
	}

	var result jsonError
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	if result.Error != "1/2 poll jobs failed" || result.JobID == "" {
		t.Fatalf("response = %#v", result)
	}

	run, ok := tracker.Get(result.JobID)
	if !ok || run.Status != deploymentRunStatusFailed || run.Message != "1/2 poll jobs failed" {
		t.Fatalf("tracked run = %#v, found = %t", run, ok)
	}
}

func TestHandlerData_TriggerPollHandlerRejectsAsyncWorkDuringShutdown(t *testing.T) {
	background := newBackgroundWork()
	background.CloseAndWait()
	h := &handlerData{
		appConfig:      &app.Config{ApiSecret: "poll-secret", MaxPayloadSize: 1024}, // #nosec G101 -- test fixture.
		backgroundCtx:  t.Context(),
		backgroundWork: background,
		log:            logger.New(logger.LevelCritical),
		runTracker:     newDeploymentRunTracker(nil),
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, apiPath+"/poll/run?wait=false", strings.NewReader(`[{"url":"`+validPollSourceURL+`"}]`))
	request.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)

	h.TriggerPollHandler(response, request)

	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), errBackgroundWorkClosed.Error()) {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true

	return nil
}

func TestHandlerData_TriggerScheduledJobHandlerValidation(t *testing.T) {
	t.Parallel()

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

	h := handlerData{
		appConfig: appConfig,
		log:       logger.New(logger.LevelCritical),
	}

	tests := []struct {
		name           string
		method         string
		setAPIKey      bool
		expectedStatus int
	}{
		{
			name:           "invalid method",
			method:         http.MethodGet,
			setAPIKey:      true,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "missing api key",
			method:         http.MethodPost,
			setAPIKey:      false,
			expectedStatus: http.StatusUnauthorized,
		},
	}

	endpoint := path.Join(apiPath, "/job/{jobName}/run")
	requestPath := path.Join(apiPath, "/job/example-job/run")

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(tc.method, requestPath, nil)
			if err != nil {
				t.Fatal(err)
			}

			if tc.setAPIKey {
				req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)
			}

			rr := httptest.NewRecorder()
			mux := http.NewServeMux()
			mux.HandleFunc(endpoint, h.TriggerScheduledJobHandler)
			mux.ServeHTTP(rr, req)

			if rr.Code != tc.expectedStatus {
				t.Fatalf("handler returned wrong status code: got %v want %v", rr.Code, tc.expectedStatus)
			}
		})
	}
}

func TestTriggerScheduledJobSyncTracksResult(t *testing.T) {
	tests := []struct {
		name       string
		triggerErr error
		wantStatus deploymentRunStatus
		wantErr    error
	}{
		{name: "success", wantStatus: deploymentRunStatusSucceeded},
		{name: "not found", triggerErr: scheduler.ErrScheduledJobNotFound, wantStatus: deploymentRunStatusFailed, wantErr: scheduler.ErrScheduledJobNotFound},
		{name: "disabled conflict", triggerErr: scheduler.ErrScheduledJobDisabled, wantStatus: deploymentRunStatusFailed, wantErr: scheduler.ErrScheduledJobDisabled},
		{name: "ambiguous conflict", triggerErr: scheduler.ErrScheduledJobAmbiguous, wantStatus: deploymentRunStatusFailed, wantErr: scheduler.ErrScheduledJobAmbiguous},
		{name: "internal", triggerErr: errors.New("trigger failed"), wantStatus: deploymentRunStatusFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tracker := newDeploymentRunTracker(nil)
			h := handlerData{
				log:        logger.New(logger.LevelCritical),
				runTracker: tracker,
				triggerScheduledJob: func(context.Context, command.Cli, *slog.Logger, string, string, *secretprovider.SecretProvider) (string, error) {
					return "scheduled-run-id", tc.triggerErr
				},
			}

			jobID, err := h.triggerScheduledJobRun(t.Context(), "deployment-job-id", h.dockerCli, docker.DisplayContextName(""), "backup", "prod", true)
			if jobID != "deployment-job-id" {
				t.Fatalf("job ID = %q, want deployment-job-id", jobID)
			}

			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, tc.wantErr)
			}

			if tc.wantErr == nil && tc.triggerErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.triggerErr != nil && err == nil {
				t.Fatal("expected trigger error")
			}

			run, ok := tracker.Get(jobID)
			if !ok {
				t.Fatalf("tracked run %q not found", jobID)
			}

			if run.Status != tc.wantStatus || run.Repository != "scheduled:backup" || run.Target != "prod" {
				t.Fatalf("unexpected tracked run: %#v", run)
			}

			if tc.triggerErr == nil && run.Message != "scheduled job trigger completed" {
				t.Fatalf("success message = %q", run.Message)
			}
		})
	}
}

func TestTriggerScheduledJobWorksWithoutTracker(t *testing.T) {
	h := handlerData{
		log: logger.New(logger.LevelCritical),
		triggerScheduledJob: func(context.Context, command.Cli, *slog.Logger, string, string, *secretprovider.SecretProvider) (string, error) {
			return "scheduled-run-id", nil
		},
	}

	jobID, err := h.triggerScheduledJobRun(t.Context(), "", h.dockerCli, docker.DisplayContextName(""), "backup", "", true)
	if err != nil || jobID == "" {
		t.Fatalf("job ID = %q, error = %v", jobID, err)
	}
}

func TestTriggerScheduledJobSyncPanicMarksFailedAndReturnsError(t *testing.T) {
	tracker := newDeploymentRunTracker(nil)
	h := handlerData{
		log:        logger.New(logger.LevelCritical),
		runTracker: tracker,
		triggerScheduledJob: func(context.Context, command.Cli, *slog.Logger, string, string, *secretprovider.SecretProvider) (string, error) {
			panic("boom")
		},
	}

	jobID, err := h.triggerScheduledJobRun(t.Context(), "deployment-job-id", h.dockerCli, docker.DisplayContextName(""), "backup", "", true)
	if err == nil {
		t.Fatal("expected internal error after scheduled job panic")
	}

	if !errors.Is(err, errScheduledJobRunPanicked) {
		t.Fatalf("error = %v, want errors.Is(_, errScheduledJobRunPanicked)", err)
	}

	if err.Error() != "scheduled job run panicked" {
		t.Fatalf("panic error exposed recovered value: %q", err)
	}

	if errors.Is(err, scheduler.ErrScheduledJobNotFound) || errors.Is(err, scheduler.ErrScheduledJobDisabled) || errors.Is(err, scheduler.ErrScheduledJobAmbiguous) {
		t.Fatalf("panic error must remain internally classified: %v", err)
	}

	run, ok := tracker.Get(jobID)
	if !ok {
		t.Fatalf("tracked run %q not found", jobID)
	}

	if run.Status != deploymentRunStatusFailed || run.Message != "scheduled job run panicked" {
		t.Fatalf("unexpected tracked run after panic: %#v", run)
	}
}

func TestTriggerScheduledJobAsyncLifecycleWaitsBeforeResourceClose(t *testing.T) {
	appCtx, appCancel := context.WithCancel(t.Context())
	defer appCancel()

	started := make(chan struct{})
	release := make(chan struct{})
	resourceClosed := make(chan struct{})
	tracker := newDeploymentRunTracker(nil)
	backgroundWG := &sync.WaitGroup{}
	h := handlerData{
		backgroundCtx: appCtx,
		backgroundWG:  backgroundWG,
		log:           logger.New(logger.LevelCritical),
		runTracker:    tracker,
		triggerScheduledJob: func(ctx context.Context, _ command.Cli, _ *slog.Logger, _, _ string, _ *secretprovider.SecretProvider) (string, error) {
			close(started)
			<-release

			if ctx.Err() != nil {
				return "", ctx.Err()
			}

			return "scheduled-run-id", nil
		},
	}

	requestCtx, requestCancel := context.WithCancel(t.Context())

	jobID, err := h.triggerScheduledJobRun(requestCtx, "", h.dockerCli, docker.DisplayContextName(""), "backup", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async trigger to start")
	}

	requestCancel()

	go func() {
		backgroundWG.Wait()
		close(resourceClosed)
	}()

	select {
	case <-resourceClosed:
		t.Fatal("resource closed before scheduled job completed")
	default:
	}

	close(release)

	select {
	case <-resourceClosed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lifecycle-owned scheduled job")
	}

	waitForDeploymentRunStatus(t, tracker, jobID, deploymentRunStatusSucceeded)
}

func TestTriggerScheduledJobAsyncLifecycleCancellationMarksFailed(t *testing.T) {
	appCtx, appCancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	tracker := newDeploymentRunTracker(nil)
	backgroundWG := &sync.WaitGroup{}
	h := handlerData{
		backgroundCtx: appCtx,
		backgroundWG:  backgroundWG,
		log:           logger.New(logger.LevelCritical),
		runTracker:    tracker,
		triggerScheduledJob: func(ctx context.Context, _ command.Cli, _ *slog.Logger, _, _ string, _ *secretprovider.SecretProvider) (string, error) {
			close(started)
			<-ctx.Done()

			return "", ctx.Err()
		},
	}

	jobID, err := h.triggerScheduledJobRun(t.Context(), "", h.dockerCli, docker.DisplayContextName(""), "backup", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async trigger to start")
	}

	appCancel()
	backgroundWG.Wait()

	run := waitForDeploymentRunStatus(t, tracker, jobID, deploymentRunStatusFailed)
	if !strings.Contains(run.Message, context.Canceled.Error()) {
		t.Fatalf("cancellation failure message = %q", run.Message)
	}
}

func TestTriggerScheduledJobHandlerMapsSchedulerErrors(t *testing.T) {
	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		triggerErr error
		wantStatus int
		wantBody   string
	}{
		{name: "not found", triggerErr: scheduler.ErrScheduledJobNotFound, wantStatus: http.StatusNotFound, wantBody: scheduler.ErrScheduledJobNotFound.Error()},
		{name: "disabled", triggerErr: scheduler.ErrScheduledJobDisabled, wantStatus: http.StatusConflict, wantBody: scheduler.ErrScheduledJobDisabled.Error()},
		{name: "ambiguous", triggerErr: scheduler.ErrScheduledJobAmbiguous, wantStatus: http.StatusConflict, wantBody: scheduler.ErrScheduledJobAmbiguous.Error()},
		{name: "internal", triggerErr: errors.New("trigger failed"), wantStatus: http.StatusInternalServerError, wantBody: "failed to trigger scheduled job run"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := handlerData{
				appConfig: appConfig,
				log:       logger.New(logger.LevelCritical),
				triggerScheduledJob: func(context.Context, command.Cli, *slog.Logger, string, string, *secretprovider.SecretProvider) (string, error) {
					return "scheduled-run-id", tc.triggerErr
				},
			}

			endpoint := path.Join(apiPath, "/job/{jobName}/run")
			requestPath := path.Join(apiPath, "/job/example-job/run")
			req := httptest.NewRequest(http.MethodPost, requestPath, nil)
			req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)

			rr := httptest.NewRecorder()
			mux := http.NewServeMux()
			mux.HandleFunc(endpoint, h.TriggerScheduledJobHandler)
			mux.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus || !strings.Contains(rr.Body.String(), tc.wantBody) {
				t.Fatalf("status = %d, body = %q; want status %d containing %q", rr.Code, rr.Body.String(), tc.wantStatus, tc.wantBody)
			}
		})
	}
}

func TestTriggerScheduledJobHandlerRejectsAsyncWorkDuringShutdown(t *testing.T) {
	background := newBackgroundWork()
	background.CloseAndWait()
	h := handlerData{
		appConfig:      &app.Config{ApiSecret: "job-secret"}, // #nosec G101 -- test fixture.
		backgroundCtx:  t.Context(),
		backgroundWork: background,
		log:            logger.New(logger.LevelCritical),
		runTracker:     newDeploymentRunTracker(nil),
	}
	req := httptest.NewRequest(http.MethodPost, apiPath+"/job/example-job/run?wait=false", nil)
	req.SetPathValue("jobName", "example-job")
	req.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)

	rr := httptest.NewRecorder()

	h.TriggerScheduledJobHandler(rr, req)

	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), errBackgroundWorkClosed.Error()) {
		t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
	}
}

func TestTriggerScheduledJobAsyncTracksAcceptedThenTerminal(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	tracker := newDeploymentRunTracker(nil)
	backgroundWG := &sync.WaitGroup{}
	h := handlerData{
		backgroundCtx: t.Context(),
		backgroundWG:  backgroundWG,
		log:           logger.New(logger.LevelCritical),
		runTracker:    tracker,
		triggerScheduledJob: func(context.Context, command.Cli, *slog.Logger, string, string, *secretprovider.SecretProvider) (string, error) {
			close(started)
			<-release
			close(finished)

			return "scheduled-run-id", nil
		},
	}

	jobID, err := h.triggerScheduledJobRun(t.Context(), "", h.dockerCli, docker.DisplayContextName(""), "backup", "prod", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if jobID == "" {
		t.Fatal("expected generated job ID")
	}

	run, ok := tracker.Get(jobID)
	if !ok || run.Status != deploymentRunStatusAccepted && run.Status != deploymentRunStatusRunning {
		t.Fatalf("async run must be immediately resolvable as accepted or running: %#v, %t", run, ok)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async trigger to start")
	}

	close(release)

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async trigger to finish")
	}

	waitForDeploymentRunStatus(t, tracker, jobID, deploymentRunStatusSucceeded)
	backgroundWG.Wait()
}

func TestTriggerScheduledJobAsyncRejectsWorkDuringShutdown(t *testing.T) {
	tracker := newDeploymentRunTracker(nil)
	background := newBackgroundWork()
	background.CloseAndWait()
	h := handlerData{
		backgroundCtx:  t.Context(),
		backgroundWork: background,
		log:            logger.New(logger.LevelCritical),
		runTracker:     tracker,
		triggerScheduledJob: func(context.Context, command.Cli, *slog.Logger, string, string, *secretprovider.SecretProvider) (string, error) {
			t.Fatal("scheduled job started during shutdown")

			return "", nil
		},
	}

	jobID, err := h.triggerScheduledJobRun(t.Context(), "", h.dockerCli, docker.DisplayContextName(""), "backup", "prod", false)
	if !errors.Is(err, errBackgroundWorkClosed) {
		t.Fatalf("error = %v, want %v", err, errBackgroundWorkClosed)
	}

	run, ok := tracker.Get(jobID)
	if !ok || run.Status != deploymentRunStatusFailed || run.Message != errBackgroundWorkClosed.Error() {
		t.Fatalf("tracked run = %#v, found = %t", run, ok)
	}
}

func TestTriggerScheduledJobAsyncPanicMarksFailed(t *testing.T) {
	tracker := newDeploymentRunTracker(nil)
	backgroundWG := &sync.WaitGroup{}
	h := handlerData{
		backgroundCtx: t.Context(),
		backgroundWG:  backgroundWG,
		log:           logger.New(logger.LevelCritical),
		runTracker:    tracker,
		triggerScheduledJob: func(context.Context, command.Cli, *slog.Logger, string, string, *secretprovider.SecretProvider) (string, error) {
			panic("boom")
		},
	}

	jobID, err := h.triggerScheduledJobRun(t.Context(), "", h.dockerCli, docker.DisplayContextName(""), "backup", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	run := waitForDeploymentRunStatus(t, tracker, jobID, deploymentRunStatusFailed)
	if run.Message != "scheduled job run panicked" {
		t.Fatalf("panic failure message = %q", run.Message)
	}

	backgroundWG.Wait()
}

func TestTriggerScheduledJobHandlerMalformedWaitDoesNotTrigger(t *testing.T) {
	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	triggered := false
	h := handlerData{
		appConfig: appConfig,
		log:       logger.New(logger.LevelCritical),
		triggerScheduledJob: func(context.Context, command.Cli, *slog.Logger, string, string, *secretprovider.SecretProvider) (string, error) {
			triggered = true

			return "", nil
		},
	}

	endpoint := path.Join(apiPath, "/job/{jobName}/run")
	requestPath := path.Join(apiPath, "/job/example-job/run") + "?wait=invalid"
	req := httptest.NewRequest(http.MethodPost, requestPath, nil)
	req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)

	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc(endpoint, h.TriggerScheduledJobHandler)
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}

	if triggered {
		t.Fatal("scheduled job triggered after malformed wait query")
	}
}

func waitForDeploymentRunStatus(t *testing.T, tracker *deploymentRunTracker, jobID string, want deploymentRunStatus) deploymentRun {
	t.Helper()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	for {
		run, ok := tracker.Get(jobID)
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

func TestHandlerData_GetScheduledJobsHandlerValidation(t *testing.T) {
	t.Parallel()

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

	h := handlerData{
		appConfig: appConfig,
		log:       logger.New(logger.LevelCritical),
	}

	tests := []struct {
		name           string
		method         string
		setAPIKey      bool
		expectedStatus int
	}{
		{
			name:           "invalid method",
			method:         http.MethodPost,
			setAPIKey:      true,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "missing api key",
			method:         http.MethodGet,
			setAPIKey:      false,
			expectedStatus: http.StatusUnauthorized,
		},
	}

	endpoint := path.Join(apiPath, "/jobs")

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(tc.method, endpoint, nil)
			if err != nil {
				t.Fatal(err)
			}

			if tc.setAPIKey {
				req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)
			}

			rr := httptest.NewRecorder()
			mux := http.NewServeMux()
			mux.HandleFunc(endpoint, h.GetScheduledJobsHandler)
			mux.ServeHTTP(rr, req)

			if rr.Code != tc.expectedStatus {
				t.Fatalf("handler returned wrong status code: got %v want %v", rr.Code, tc.expectedStatus)
			}
		})
	}
}

func TestHandlerData_DeploymentRunHandlers(t *testing.T) {
	t.Parallel()

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

	tracker := newDeploymentRunTracker(map[deploymentRunTrigger]int{
		deploymentRunTriggerWebhook:      10,
		deploymentRunTriggerPoll:         10,
		deploymentRunTriggerScheduledJob: 10,
	})
	tracker.TrackAccepted("job-1", deploymentRunTriggerWebhook)
	tracker.SetMetadata("job-1", "owner/repo", "prod", "refs/heads/main")
	tracker.MarkSucceeded("job-1", "ok")

	h := handlerData{
		appConfig:      appConfig,
		log:            logger.New(logger.LevelCritical),
		runTracker:     tracker,
		secretProvider: nil,
	}

	t.Run("list runs", func(t *testing.T) {
		t.Parallel()

		req, err := http.NewRequest(http.MethodGet, path.Join(apiPath, "/runs?limit=5&status=succeeded&trigger=webhook"), nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)

		rr := httptest.NewRecorder()
		mux := http.NewServeMux()
		mux.HandleFunc(path.Join(apiPath, "/runs"), h.GetDeploymentRunsHandler)
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
		}

		if !strings.Contains(rr.Body.String(), `"job_id":"job-1"`) {
			t.Fatalf("expected response to contain run job id, got %s", rr.Body.String())
		}
	})

	t.Run("get one run", func(t *testing.T) {
		t.Parallel()

		req, err := http.NewRequest(http.MethodGet, path.Join(apiPath, "/run/job-1"), nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)

		rr := httptest.NewRecorder()
		mux := http.NewServeMux()
		mux.HandleFunc(path.Join(apiPath, "/run/{jobID}"), h.GetDeploymentRunHandler)
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
		}

		if !strings.Contains(rr.Body.String(), `"status":"succeeded"`) {
			t.Fatalf("expected succeeded run in response, got %s", rr.Body.String())
		}
	})
}
