package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"

	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

func TestGetScheduledJobsLastRunContract(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status controlplane.RunStatus
		runID  string
		evict  bool
	}{
		{name: "never run"},
		{name: "unavailable history", runID: "missing-run"},
		{name: "evicted history", status: controlplane.RunStatusSucceeded, runID: "evicted-run", evict: true},
		{name: "running", status: controlplane.RunStatusRunning, runID: "running-run"},
		{name: "succeeded", status: controlplane.RunStatusSucceeded, runID: "successful-run"},
		{name: "failed", status: controlplane.RunStatusFailed, runID: "failed-run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			startedAt := time.Now().In(time.Local).Add(-time.Minute)
			nextRunAt := startedAt.Add(time.Hour)
			job := scheduler.JobInfo{
				LatestRunID: tc.runID, Name: "backup", Context: "default", Stack: "prod", Mode: "container",
				Schedule: "@every 1h", ExecutionMode: docker.JobExecutionModeOneOff, NotifyOn: docker.JobNotifyAll,
				Status: "exited (0)", Repository: "owner/repo", StopServices: []string{"app"}, Replicas: 1,
				Enabled: true, SkipRunning: true, Valid: true, LastRunAt: &startedAt,
				NextRunAt: &nextRunAt, LabelNextRunAt: &nextRunAt,
			}

			runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				maxRunsPerTrigger: map[controlplane.RunTrigger]int{controlplane.RunTriggerScheduledJob: 1},
				scheduledJobs: testScheduledJobOperations{listJobs: func(_ context.Context, contextName, stackName string) ([]scheduler.JobInfo, error) {
					if contextName != "default" || stackName != "prod" {
						t.Errorf("list filters = %q, %q", contextName, stackName)
					}

					return []scheduler.JobInfo{job}, nil
				}},
			})
			if tc.status != "" {
				runs.ScheduledRunStarted(scheduler.RunExecution{
					RunID: tc.runID, JobName: job.Name, Context: job.Context, Stack: job.Stack, Mode: job.Mode, StartedAt: startedAt,
				})
				runs.SetMetadata(tc.runID, controlplane.RunMetadata{Repository: job.Repository, Target: job.Stack, Revision: "main"})

				switch tc.status {
				case controlplane.RunStatusSucceeded:
					runs.ScheduledRunFinished(tc.runID, nil)
				case controlplane.RunStatusFailed:
					runs.ScheduledRunFinished(tc.runID, errors.New("exit code 7"))
				}
			}

			if tc.evict {
				runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "newer-run", JobName: "other", StartedAt: time.Now().UTC()})
				runs.ScheduledRunFinished("newer-run", nil)

				if _, ok := runs.Get(tc.runID); ok {
					t.Fatal("eviction fixture still has the original run")
				}
			}

			h := Handler{appConfig: &app.Config{ApiSecret: "test-api-secret"}, log: logger.New(logger.LevelCritical), controlPlaneRuns: runs}
			mux := http.NewServeMux()
			mux.HandleFunc(APIPath+"/jobs", h.GetScheduledJobsHandler)
			mux.HandleFunc(APIPath+"/run/{jobID}", h.GetDeploymentRunHandler)

			get := func(endpoint string) map[string]json.RawMessage {
				t.Helper()

				req := httptest.NewRequest(http.MethodGet, endpoint, nil)
				req.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)

				response := httptest.NewRecorder()
				mux.ServeHTTP(response, req)

				if response.Code != http.StatusOK {
					t.Fatalf("GET %s: status %d, body %s", endpoint, response.Code, response.Body.String())
				}

				if endpoint == APIPath+"/jobs?context=default&stack=prod" && response.Header().Get(dockerContextHeader) != "default" {
					t.Fatalf("context header = %q", response.Header().Get(dockerContextHeader))
				}

				var envelope map[string]json.RawMessage
				if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}

				if len(envelope["job_id"]) == 0 {
					t.Fatal("response lost its request job_id envelope")
				}

				return envelope
			}
			envelope := get(APIPath + "/jobs?context=default&stack=prod")

			var jobs []map[string]json.RawMessage
			if err := json.Unmarshal(envelope["content"], &jobs); err != nil || len(jobs) != 1 {
				t.Fatalf("decode jobs: %v, content %s", err, envelope["content"])
			}

			lastRun, present := jobs[0]["last_run"]
			if !present {
				t.Fatal("last_run must always be serialized")
			}

			if tc.status == "" || tc.evict {
				if string(lastRun) != "null" {
					t.Fatalf("last_run = %s, want null even when last_run_at exists", lastRun)
				}
			} else {
				detail := get(APIPath + "/run/" + tc.runID)
				if !reflect.DeepEqual(lastRun, detail["content"]) {
					t.Fatalf("last_run %s differs from complete run detail %s", lastRun, detail["content"])
				}
			}

			delete(jobs[0], "last_run")

			encodedJob, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}

			var original map[string]json.RawMessage
			if err := json.Unmarshal(encodedJob, &original); err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(jobs[0], original) {
				t.Fatalf("existing flat job fields changed: got %#v, want %#v", jobs[0], original)
			}

			for _, hidden := range []string{"LatestRunID", "latest_run_id", "last_run_id", "JobInfo"} {
				if _, ok := jobs[0][hidden]; ok {
					t.Fatalf("job leaked internal or redundant field %q", hidden)
				}
			}
		})
	}
}

func TestHandler_TriggerScheduledJobHandlerValidation(t *testing.T) {
	t.Parallel()

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

	h := Handler{
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

	endpoint := path.Join(APIPath, "/job/{jobName}/run")
	requestPath := path.Join(APIPath, "/job/example-job/run")

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
		wantStatus controlplane.RunStatus
		wantErr    error
	}{
		{name: "success", wantStatus: controlplane.RunStatusSucceeded},
		{name: "not found", triggerErr: scheduler.ErrScheduledJobNotFound, wantStatus: controlplane.RunStatusFailed, wantErr: scheduler.ErrScheduledJobNotFound},
		{name: "disabled conflict", triggerErr: scheduler.ErrScheduledJobDisabled, wantStatus: controlplane.RunStatusFailed, wantErr: scheduler.ErrScheduledJobDisabled},
		{name: "ambiguous conflict", triggerErr: scheduler.ErrScheduledJobAmbiguous, wantStatus: controlplane.RunStatusFailed, wantErr: scheduler.ErrScheduledJobAmbiguous},
		{name: "internal", triggerErr: errors.New("trigger failed"), wantStatus: controlplane.RunStatusFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := logger.New(logger.LevelCritical)
			runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				log: log,
				scheduledJobs: testScheduledJobOperations{triggerNow: func(
					context.Context,
					string,
					string,
					string,
					secretprovider.SecretProvider,
				) (string, error) {
					return "scheduled-run-id", tc.triggerErr
				}},
			})

			jobID, err := runs.TriggerScheduledJob(t.Context(), "deployment-job-id", "default", "backup", "prod", true)
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

			run, ok := runs.Get(jobID)
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

func TestTriggerScheduledJobGeneratesJobID(t *testing.T) {
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		scheduledJobs: testScheduledJobOperations{triggerNow: func(
			context.Context,
			string,
			string,
			string,
			secretprovider.SecretProvider,
		) (string, error) {
			return "scheduled-run-id", nil
		}},
	})

	jobID, err := runs.TriggerScheduledJob(t.Context(), "", "default", "backup", "", true)
	if err != nil || jobID == "" {
		t.Fatalf("job ID = %q, error = %v", jobID, err)
	}
}

func TestTriggerScheduledJobSyncPanicMarksFailedAndReturnsError(t *testing.T) {
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		scheduledJobs: testScheduledJobOperations{triggerNow: func(
			context.Context,
			string,
			string,
			string,
			secretprovider.SecretProvider,
		) (string, error) {
			panic("boom")
		}},
	})

	jobID, err := runs.TriggerScheduledJob(t.Context(), "deployment-job-id", "default", "backup", "", true)
	if err == nil {
		t.Fatal("expected internal error after scheduled job panic")
	}

	if !errors.Is(err, controlplane.ErrScheduledJobRunPanicked) {
		t.Fatalf("error = %v, want errors.Is(_, ErrScheduledJobRunPanicked)", err)
	}

	if err.Error() != "scheduled job run panicked" {
		t.Fatalf("panic error exposed recovered value: %q", err)
	}

	if errors.Is(err, scheduler.ErrScheduledJobNotFound) || errors.Is(err, scheduler.ErrScheduledJobDisabled) || errors.Is(err, scheduler.ErrScheduledJobAmbiguous) {
		t.Fatalf("panic error must remain internally classified: %v", err)
	}

	run, ok := runs.Get(jobID)
	if !ok {
		t.Fatalf("tracked run %q not found", jobID)
	}

	if run.Status != controlplane.RunStatusFailed || run.Message != "scheduled job run panicked" {
		t.Fatalf("unexpected tracked run after panic: %#v", run)
	}
}

func TestTriggerScheduledJobAsyncLifecycleWaitsBeforeResourceClose(t *testing.T) {
	appCtx, appCancel := context.WithCancel(t.Context())
	defer appCancel()

	started := make(chan struct{})
	release := make(chan struct{})
	resourceClosed := make(chan struct{})
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		applicationCtx: appCtx,
		scheduledJobs: testScheduledJobOperations{triggerNow: func(
			ctx context.Context,
			_ string,
			_ string,
			_ string,
			_ secretprovider.SecretProvider,
		) (string, error) {
			close(started)
			<-release

			if ctx.Err() != nil {
				return "", ctx.Err()
			}

			return "scheduled-run-id", nil
		}},
	})

	requestCtx, requestCancel := context.WithCancel(t.Context())

	jobID, err := runs.TriggerScheduledJob(requestCtx, "", "default", "backup", "", false)
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
		for {
			run, ok := runs.Get(jobID)
			if ok && (run.Status == controlplane.RunStatusSucceeded || run.Status == controlplane.RunStatusFailed) {
				close(resourceClosed)

				return
			}

			time.Sleep(time.Millisecond)
		}
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

	waitForDeploymentRunStatus(t, runs, jobID, controlplane.RunStatusSucceeded)
}

func TestTriggerScheduledJobAsyncLifecycleCancellationMarksFailed(t *testing.T) {
	appCtx, appCancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		applicationCtx: appCtx,
		scheduledJobs: testScheduledJobOperations{triggerNow: func(
			ctx context.Context,
			_ string,
			_ string,
			_ string,
			_ secretprovider.SecretProvider,
		) (string, error) {
			close(started)
			<-ctx.Done()

			return "", ctx.Err()
		}},
	})

	jobID, err := runs.TriggerScheduledJob(t.Context(), "", "default", "backup", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async trigger to start")
	}

	appCancel()

	run := waitForDeploymentRunStatus(t, runs, jobID, controlplane.RunStatusFailed)
	if !strings.Contains(run.Message, context.Canceled.Error()) {
		t.Fatalf("cancellation failure message = %q", run.Message)
	}
}

func TestTriggerScheduledJobHandlerMapsSchedulerErrors(t *testing.T) {
	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

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
			log := logger.New(logger.LevelCritical)
			h := Handler{
				appConfig: appConfig,
				log:       log,
				controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
					log: log,
					scheduledJobs: testScheduledJobOperations{triggerNow: func(
						context.Context,
						string,
						string,
						string,
						secretprovider.SecretProvider,
					) (string, error) {
						return "scheduled-run-id", tc.triggerErr
					}},
				}),
			}

			endpoint := path.Join(APIPath, "/job/{jobName}/run")
			requestPath := path.Join(APIPath, "/job/example-job/run") + tc.query
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
	log := logger.New(logger.LevelCritical)
	h := Handler{
		appConfig: &app.Config{ApiSecret: "job-secret"}, // #nosec G101 -- test fixture.
		log:       log,
		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			closed: true,
			log:    log,
			scheduledJobs: testScheduledJobOperations{triggerNow: func(
				context.Context,
				string,
				string,
				string,
				secretprovider.SecretProvider,
			) (string, error) {
				t.Fatal("scheduled job started during shutdown")

				return "", nil
			}},
		}),
	}
	req := httptest.NewRequest(http.MethodPost, APIPath+"/job/example-job/run?wait=false", nil)
	req.SetPathValue("jobName", "example-job")
	req.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)

	rr := httptest.NewRecorder()

	h.TriggerScheduledJobHandler(rr, req)

	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), controlplane.ErrBackgroundWorkClosed.Error()) {
		t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
	}
}

func TestTriggerScheduledJobAsyncTracksAcceptedThenTerminal(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		scheduledJobs: testScheduledJobOperations{triggerNow: func(
			context.Context,
			string,
			string,
			string,
			secretprovider.SecretProvider,
		) (string, error) {
			close(started)
			<-release
			close(finished)

			return "scheduled-run-id", nil
		}},
	})

	jobID, err := runs.TriggerScheduledJob(t.Context(), "", "default", "backup", "prod", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if jobID == "" {
		t.Fatal("expected generated job ID")
	}

	run, ok := runs.Get(jobID)
	if !ok || run.Status != controlplane.RunStatusAccepted && run.Status != controlplane.RunStatusRunning {
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

	waitForDeploymentRunStatus(t, runs, jobID, controlplane.RunStatusSucceeded)
}

func TestTriggerScheduledJobAsyncRejectsWorkDuringShutdown(t *testing.T) {
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		closed: true,
		scheduledJobs: testScheduledJobOperations{triggerNow: func(
			context.Context,
			string,
			string,
			string,
			secretprovider.SecretProvider,
		) (string, error) {
			t.Fatal("scheduled job started during shutdown")

			return "", nil
		}},
	})

	jobID, err := runs.TriggerScheduledJob(t.Context(), "", "default", "backup", "prod", false)
	if !errors.Is(err, controlplane.ErrBackgroundWorkClosed) {
		t.Fatalf("error = %v, want %v", err, controlplane.ErrBackgroundWorkClosed)
	}

	run, ok := runs.Get(jobID)
	if !ok || run.Status != controlplane.RunStatusFailed || run.Message != controlplane.ErrBackgroundWorkClosed.Error() {
		t.Fatalf("tracked run = %#v, found = %t", run, ok)
	}
}

func TestTriggerScheduledJobAsyncPanicMarksFailed(t *testing.T) {
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		scheduledJobs: testScheduledJobOperations{triggerNow: func(
			context.Context,
			string,
			string,
			string,
			secretprovider.SecretProvider,
		) (string, error) {
			panic("boom")
		}},
	})

	jobID, err := runs.TriggerScheduledJob(t.Context(), "", "default", "backup", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	run := waitForDeploymentRunStatus(t, runs, jobID, controlplane.RunStatusFailed)
	if run.Message != "scheduled job run panicked" {
		t.Fatalf("panic failure message = %q", run.Message)
	}
}

func TestTriggerScheduledJobHandlerMalformedWaitDoesNotTrigger(t *testing.T) {
	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

	triggered := false
	log := logger.New(logger.LevelCritical)
	h := Handler{
		appConfig: appConfig,
		log:       log,
		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			log: log,
			scheduledJobs: testScheduledJobOperations{triggerNow: func(
				context.Context,
				string,
				string,
				string,
				secretprovider.SecretProvider,
			) (string, error) {
				triggered = true

				return "", nil
			}},
		}),
	}

	endpoint := path.Join(APIPath, "/job/{jobName}/run")
	requestPath := path.Join(APIPath, "/job/example-job/run") + "?wait=invalid"
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

func TestHandler_GetScheduledJobsHandlerValidation(t *testing.T) {
	t.Parallel()

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

	h := Handler{
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

	endpoint := path.Join(APIPath, "/jobs")

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
