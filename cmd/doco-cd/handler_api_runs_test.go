package main

import (
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"

	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
)

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
		appConfig: appConfig,
		log:       logger.New(logger.LevelCritical),
	}
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		tracker: tracker,
		log:     h.log,
	})

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
