package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/controlplane"

	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
)

func TestHandler_DeploymentRunHandlers(t *testing.T) {
	t.Parallel()

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		maxRunsPerTrigger: map[controlplane.RunTrigger]int{
			controlplane.RunTriggerWebhook:      10,
			controlplane.RunTriggerPoll:         10,
			controlplane.RunTriggerScheduledJob: 10,
		},
	})
	runs.Accept("job-1", controlplane.RunTriggerWebhook, controlplane.RunMetadata{
		Repository: "owner/repo",
		Target:     "prod",
		Revision:   "refs/heads/main",
	})

	if err := runs.Execute(t.Context(), "job-1", controlplane.RunExecution{
		Mode:         controlplane.RunSynchronous,
		PanicContext: "seed run",
		PanicError:   errors.New("seed run panicked"),
	}, func(context.Context) (controlplane.RunResult, error) {
		return controlplane.SucceededRun("ok"), nil
	}); err != nil {
		t.Fatal(err)
	}

	h := Handler{
		appConfig: appConfig,
		log:       logger.New(logger.LevelCritical),

		controlPlaneRuns: runs,
	}

	t.Run("list runs", func(t *testing.T) {
		t.Parallel()

		req, err := http.NewRequest(http.MethodGet, path.Join(APIPath, "/runs?limit=5&status=succeeded&trigger=webhook"), nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)

		rr := httptest.NewRecorder()
		mux := http.NewServeMux()
		mux.HandleFunc(path.Join(APIPath, "/runs"), h.GetDeploymentRunsHandler)
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

		req, err := http.NewRequest(http.MethodGet, path.Join(APIPath, "/run/job-1"), nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)

		rr := httptest.NewRecorder()
		mux := http.NewServeMux()
		mux.HandleFunc(path.Join(APIPath, "/run/{jobID}"), h.GetDeploymentRunHandler)
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
		}

		if !strings.Contains(rr.Body.String(), `"status":"succeeded"`) {
			t.Fatalf("expected succeeded run in response, got %s", rr.Body.String())
		}
	})
}
