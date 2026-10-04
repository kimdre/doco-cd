package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
)

const testStorageAPISecret = "test-api-secret"

// fakeCompactionRuns implements the mirror compaction part of RunOperations.
type fakeCompactionRuns struct {
	RunOperations

	trigger func(ctx context.Context, jobID string, req controlplane.MirrorCompactionRequest, wait bool) (string, error)
	runs    map[string]controlplane.Run
}

func (f *fakeCompactionRuns) TriggerMirrorCompaction(ctx context.Context, jobID string, req controlplane.MirrorCompactionRequest, wait bool) (string, error) {
	return f.trigger(ctx, jobID, req, wait)
}

func (f *fakeCompactionRuns) Get(jobID string) (controlplane.Run, bool) {
	run, ok := f.runs[jobID]

	return run, ok
}

func serveCompactMirrors(t *testing.T, runs RunOperations, method, target string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	h := Handler{
		appConfig:        &app.Config{ApiSecret: testStorageAPISecret},
		log:              logger.New(logger.LevelCritical),
		controlPlaneRuns: runs,
	}

	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	req.Header.Set(restAPI.KeyHeader, testStorageAPISecret)

	rr := httptest.NewRecorder()
	h.CompactMirrorsHandler(rr, req)

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}

	return rr, body
}

func TestCompactMirrorsHandlerPassesRequest(t *testing.T) {
	t.Parallel()

	var (
		gotReq  controlplane.MirrorCompactionRequest
		gotWait bool
	)

	runs := &fakeCompactionRuns{
		trigger: func(_ context.Context, jobID string, req controlplane.MirrorCompactionRequest, wait bool) (string, error) {
			gotReq, gotWait = req, wait

			return jobID, nil
		},
	}

	rr, body := serveCompactMirrors(t, runs, http.MethodPost,
		APIPath+"/storage/compact?repository=github.com/acme/app&mode=copy&max_size=0")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusAccepted, rr.Body.String())
	}

	if body["content"] != "mirror compaction started" || body["job_id"] == "" {
		t.Fatalf("body = %v", body)
	}

	if gotReq.Repository != "github.com/acme/app" || gotReq.Mode != git.MirrorCompactionCopy || gotWait {
		t.Fatalf("request = %+v, wait = %v", gotReq, gotWait)
	}

	if gotReq.MaxSizeBytes == nil || *gotReq.MaxSizeBytes != 0 {
		t.Fatalf("max size = %v, want 0", gotReq.MaxSizeBytes)
	}
}

func TestCompactMirrorsHandlerDefaults(t *testing.T) {
	t.Parallel()

	var gotReq controlplane.MirrorCompactionRequest

	runs := &fakeCompactionRuns{
		trigger: func(_ context.Context, jobID string, req controlplane.MirrorCompactionRequest, _ bool) (string, error) {
			gotReq = req

			return jobID, nil
		},
	}

	rr, _ := serveCompactMirrors(t, runs, http.MethodPost, APIPath+"/storage/compact")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusAccepted)
	}

	if gotReq.Repository != "" || gotReq.Mode != "" || gotReq.MaxSizeBytes != nil {
		t.Fatalf("request = %+v, want zero value", gotReq)
	}
}

func TestCompactMirrorsHandlerWaitRespondsWithRun(t *testing.T) {
	t.Parallel()

	runs := &fakeCompactionRuns{runs: map[string]controlplane.Run{}}
	runs.trigger = func(_ context.Context, jobID string, _ controlplane.MirrorCompactionRequest, wait bool) (string, error) {
		if !wait {
			t.Error("wait was not passed on")
		}

		runs.runs[jobID] = controlplane.Run{
			JobID:   jobID,
			Trigger: controlplane.RunTriggerMirrorCompaction,
			Status:  controlplane.RunStatusSkipped,
			Message: "repack of 1 mirrors: 1 skipped_single_pack; packfiles 1.0 KiB -> 1.0 KiB",
		}

		return jobID, nil
	}

	rr, body := serveCompactMirrors(t, runs, http.MethodPost, APIPath+"/storage/compact?wait=true")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	content, ok := body["content"].(map[string]any)
	if !ok {
		t.Fatalf("content = %v, want a run", body["content"])
	}

	if content["status"] != string(controlplane.RunStatusSkipped) || content["trigger"] != string(controlplane.RunTriggerMirrorCompaction) {
		t.Fatalf("run = %v", content)
	}

	if content["job_id"] != body["job_id"] {
		t.Fatalf("run job ID = %v, response job ID = %v", content["job_id"], body["job_id"])
	}
}

func TestCompactMirrorsHandlerErrors(t *testing.T) {
	t.Parallel()

	failedRun := controlplane.Run{
		Status:  controlplane.RunStatusFailed,
		Message: "repack of 1 mirrors: 1 failed; packfiles 1.0 KiB -> 1.0 KiB",
	}

	for _, testCase := range []struct {
		name       string
		method     string
		query      string
		err        error
		jobID      string
		wantStatus int
		wantJobID  string
		wantDetail string
	}{
		{name: "method", method: http.MethodGet, wantStatus: http.StatusMethodNotAllowed},
		{name: "max size", query: "?max_size=1GiB", wantStatus: http.StatusBadRequest, wantDetail: "'max_size' parameter must be an integer"},
		{name: "wait", query: "?wait=maybe", wantStatus: http.StatusBadRequest},
		{name: "invalid request", err: controlplane.ErrInvalidMirrorCompactionRequest, wantStatus: http.StatusBadRequest},
		{name: "no mirrors", err: controlplane.ErrNoMirrors, wantStatus: http.StatusNotFound},
		{
			name:       "active",
			err:        &controlplane.MirrorCompactionActiveError{JobID: "active-job"},
			jobID:      "active-job",
			wantStatus: http.StatusConflict,
			wantJobID:  "active-job",
		},
		{name: "draining", err: controlplane.ErrBackgroundWorkClosed, wantStatus: http.StatusServiceUnavailable},
		{
			name:       "cancelled",
			err:        &controlplane.MirrorCompactionsFailedError{Failed: 1, Total: 1, Cause: context.Canceled},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "failed",
			query:      "?wait=true",
			err:        &controlplane.MirrorCompactionsFailedError{Failed: 1, Total: 1, Cause: errors.New("disk full")},
			wantStatus: http.StatusInternalServerError,
			wantDetail: failedRun.Message,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			runs := &fakeCompactionRuns{runs: map[string]controlplane.Run{}}
			runs.trigger = func(_ context.Context, jobID string, _ controlplane.MirrorCompactionRequest, _ bool) (string, error) {
				if testCase.jobID != "" {
					return testCase.jobID, testCase.err
				}

				runs.runs[jobID] = failedRun

				return jobID, testCase.err
			}

			method := testCase.method
			if method == "" {
				method = http.MethodPost
			}

			rr, body := serveCompactMirrors(t, runs, method, APIPath+"/storage/compact"+testCase.query)
			if rr.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rr.Code, testCase.wantStatus, rr.Body.String())
			}

			if testCase.wantJobID != "" && body["job_id"] != testCase.wantJobID {
				t.Fatalf("job ID = %v, want %s", body["job_id"], testCase.wantJobID)
			}

			if testCase.wantDetail != "" && body["content"] != testCase.wantDetail {
				t.Fatalf("detail = %v, want %q", body["content"], testCase.wantDetail)
			}
		})
	}
}

func TestCompactMirrorsHandlerRequiresAPIKey(t *testing.T) {
	t.Parallel()

	h := Handler{
		appConfig: &app.Config{ApiSecret: testStorageAPISecret},
		log:       logger.New(logger.LevelCritical),
		controlPlaneRuns: &fakeCompactionRuns{
			trigger: func(context.Context, string, controlplane.MirrorCompactionRequest, bool) (string, error) {
				t.Error("compaction triggered without an API key")

				return "", nil
			},
		},
	}

	rr := httptest.NewRecorder()
	h.CompactMirrorsHandler(rr, httptest.NewRequestWithContext(t.Context(), http.MethodPost, APIPath+"/storage/compact", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestCompactMirrorsHandlerWithControlPlane(t *testing.T) {
	t.Parallel()

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{storageDir: t.TempDir()})

	rr, _ := serveCompactMirrors(t, runs, http.MethodPost, APIPath+"/storage/compact?mode=gc")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status = %d, want %d: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}

	rr, _ = serveCompactMirrors(t, runs, http.MethodPost, APIPath+"/storage/compact?repository=github.com/acme/app")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown repository status = %d, want %d: %s", rr.Code, http.StatusNotFound, rr.Body.String())
	}
}
