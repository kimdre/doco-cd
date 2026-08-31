package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/config/app"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

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
		query      string
		triggerErr error
		wantStatus int
		wantBody   string
	}{
		{name: "not found", triggerErr: scheduler.ErrScheduledJobNotFound, wantStatus: http.StatusNotFound, wantBody: scheduler.ErrScheduledJobNotFound.Error()},
		{name: "disabled", triggerErr: scheduler.ErrScheduledJobDisabled, wantStatus: http.StatusConflict, wantBody: scheduler.ErrScheduledJobDisabled.Error()},
		{name: "ambiguous", triggerErr: scheduler.ErrScheduledJobAmbiguous, wantStatus: http.StatusConflict, wantBody: scheduler.ErrScheduledJobAmbiguous.Error()},
		{name: "internal", triggerErr: errors.New("trigger failed"), wantStatus: http.StatusInternalServerError, wantBody: "failed to trigger scheduled job run"},
		{name: "wait success", wantStatus: http.StatusOK, wantBody: "scheduled job triggered"},
		{name: "async accepted", query: "?wait=false", wantStatus: http.StatusAccepted, wantBody: "scheduled job trigger accepted"},
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
			requestPath := path.Join(apiPath, "/job/example-job/run") + tc.query
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
