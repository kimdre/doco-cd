package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	dockerswarmtypes "github.com/moby/moby/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/config/app"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
)

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

// TestStackActionApiHandlerSuccessResponses exercises the success block of
// StackActionApiHandler for every action, with and without a service filter,
// against a fake Docker daemon that accepts service updates.
func TestStackActionApiHandlerSuccessResponses(t *testing.T) {
	services := []dockerswarmtypes.Service{
		{ID: "web-id", Spec: dockerswarmtypes.ServiceSpec{
			Annotations: dockerswarmtypes.Annotations{Name: "stack_web"},
			Mode:        dockerswarmtypes.ServiceMode{Replicated: &dockerswarmtypes.ReplicatedService{}},
		}},
		{ID: "job-id", Spec: dockerswarmtypes.ServiceSpec{
			Annotations: dockerswarmtypes.Annotations{Name: "stack_job"},
			Mode:        dockerswarmtypes.ServiceMode{ReplicatedJob: &dockerswarmtypes.ReplicatedJob{}},
		}},
	}

	for _, testCase := range []struct {
		name     string
		path     string
		wantBody string
	}{
		{name: "scale stack", path: "/stack/stack/scale?replicas=2&wait=false", wantBody: "stack scaled: stack to 2 replicas"},
		{name: "scale service", path: "/stack/stack/scale?service=web&replicas=2&wait=false", wantBody: "service scaled: web to 2 replicas"},
		{name: "restart stack", path: "/stack/stack/restart", wantBody: "stack restarted: stack"},
		{name: "restart service", path: "/stack/stack/restart?service=web", wantBody: "service restarted: stack_web"},
		{name: "run stack jobs", path: "/stack/stack/run", wantBody: "1 job(s) retriggered in stack: stack"},
		{name: "run service", path: "/stack/stack/run?service=job", wantBody: "job retriggered: stack_job"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newStackActionRESTSuccessTestHandler(t, services)
			rr := callStackActionREST(t, h, testCase.path)

			if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), testCase.wantBody) {
				t.Fatalf("response = %d %s, want 200 containing %q", rr.Code, rr.Body.String(), testCase.wantBody)
			}
		})
	}
}

// newStackActionRESTSuccessTestHandler backs stack action success tests with a
// fake Docker daemon that lists and inspects the given services and accepts
// every service update.
func newStackActionRESTSuccessTestHandler(t *testing.T, services []dockerswarmtypes.Service) *handlerData {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/services"):
			_ = json.NewEncoder(w).Encode(services)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/services/"):
			serviceName := path.Base(r.URL.Path)
			for _, service := range services {
				if service.Spec.Name == serviceName || service.ID == serviceName {
					_ = json.NewEncoder(w).Encode(service)

					return
				}
			}

			http.NotFound(w, r)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/update"):
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
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
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
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
