package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

const selfUpdateTestCommitSHA = "0123456789012345678901234567890123456789"

type postedCommitStatus struct {
	Path        string `json:"-"`
	State       string `json:"state"`
	Description string `json:"description"`
	Context     string `json:"context"`
}

// commitStatusRecorder is a Gitea-compatible API that records posted statuses.
type commitStatusRecorder struct {
	*httptest.Server

	mu     sync.Mutex
	posted []postedCommitStatus
}

func newCommitStatusRecorder(t *testing.T, beforePost ...func()) *commitStatusRecorder {
	t.Helper()

	recorder := &commitStatusRecorder{}
	recorder.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var status postedCommitStatus
		if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
			t.Errorf("decode status: %v", err)
		}

		status.Path = r.URL.Path

		if len(beforePost) > 0 {
			beforePost[0]()
		}

		recorder.mu.Lock()
		recorder.posted = append(recorder.posted, status)
		recorder.mu.Unlock()

		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(recorder.Close)

	return recorder
}

func (r *commitStatusRecorder) statuses() []postedCommitStatus {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]postedCommitStatus(nil), r.posted...)
}

func (r *commitStatusRecorder) appConfig() *app.Config {
	return &app.Config{
		GitCommitStatus: true,
		GitAccessToken:  "token",
		GitScmProvider:  string(commitstatus.ProviderGitea),
		GitScmApiUrl:    config.HttpUrl(r.URL),
	}
}

func (r *commitStatusRecorder) requireSingle(t *testing.T, state commitstatus.State) postedCommitStatus {
	t.Helper()

	posted := r.statuses()
	if len(posted) != 1 {
		t.Fatalf("posted %d commit statuses, want 1: %+v", len(posted), posted)
	}

	got := posted[0]
	if got.State != string(state) {
		t.Errorf("state = %q, want %q", got.State, state)
	}

	if want := "/api/v1/repos/owner/infra/statuses/" + selfUpdateTestCommitSHA; got.Path != want {
		t.Errorf("path = %q, want %q", got.Path, want)
	}

	if got.Context != "doco-cd/nas/doco-cd" {
		t.Errorf("context = %q, want the pending status context", got.Context)
	}

	return got
}

// handedOverRecord is a self-update whose predecessor left a pending status.
func handedOverRecord(serverURL string, state selfupdate.State) selfupdate.Record {
	return selfupdate.Record{
		ID:          "handover",
		State:       state,
		Strategy:    selfupdate.StrategyScaleOut,
		Stack:       "self",
		Predecessor: selfupdate.ContainerRef{ID: "old"},
		Successor:   selfupdate.ContainerRef{ID: "new"},
		Source: selfupdate.SourceInfo{
			CommitSHA: selfUpdateTestCommitSHA,
			CommitStatus: &selfupdate.CommitStatusInfo{
				SourceURL: serverURL + "/owner/infra.git",
				RepoURL:   serverURL + "/owner/infra",
				FullName:  "owner/infra",
				CommitSHA: selfUpdateTestCommitSHA,
				Context:   "doco-cd/nas/doco-cd",
				StartedAt: time.Now().Add(-90 * time.Second),
			},
		},
	}
}

func TestSelfUpdateReporterPostsFinalCommitStatus(t *testing.T) {
	t.Parallel()

	log := logger.New(slog.LevelError)

	for _, tt := range []struct {
		name          string
		statusEnabled bool
	}{
		{name: "success", statusEnabled: true},
		{name: "success after statuses disabled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := newCommitStatusRecorder(t)
			cfg := recorder.appConfig()
			cfg.GitCommitStatus = tt.statusEnabled
			newSelfUpdateReporter(cfg, nil).
				reportSuccess(t.Context(), log, handedOverRecord(recorder.URL, selfupdate.StateFinalising))

			got := recorder.requireSingle(t, commitstatus.StateSuccess)
			if !strings.HasPrefix(got.Description, "Successful in 1m") {
				t.Errorf("description = %q, want the duration since the deployment started", got.Description)
			}
		})
	}
}

func TestSelfUpdateReporterPinsJournaledTarget(t *testing.T) {
	gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{
		ID: "12345", PrivateKey: "test-private-key",
	})

	restoreProvider := gitInternal.SwapGitHubAppTokenProviderForTest(func(_ string, cfg gitInternal.GitHubAppConfig) (string, error) {
		if cfg.ID != "12345" {
			t.Errorf("finishing App ID = %q, want 12345", cfg.ID)
		}

		return "ghs-finishing-token", nil
	})

	t.Cleanup(func() {
		restoreProvider()
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	for _, tc := range []struct {
		name          string
		legacy        bool
		failure       bool
		timedOut      bool
		reason        string
		statusEnabled bool
		conclusion    string
		externalOnly  bool
	}{
		{name: "old journal with App credentials", legacy: true, statusEnabled: true},
		{name: "old journal after reporting disabled", legacy: true, failure: true},
		{name: "recorded check success", statusEnabled: true, conclusion: "success"},
		{name: "recorded check after reporting disabled", conclusion: "success"},
		{name: "recorded external identity after reporting disabled", externalOnly: true, conclusion: "success"},
		{name: "ordinary unhealthy", failure: true, reason: "new reported unhealthy", statusEnabled: true, conclusion: "failure"},
		{name: "deadline text is not typed timeout", failure: true, reason: "context deadline exceeded", statusEnabled: true, conclusion: "failure"},
		{name: "structured timeout without deadline text", failure: true, timedOut: true, reason: "health gate expired", statusEnabled: true, conclusion: "timed_out"},
		{name: "timeout after reporting disabled", failure: true, timedOut: true, conclusion: "timed_out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type postedReport struct {
				postedCommitStatus
				Method      string    `json:"-"`
				Name        string    `json:"name"`
				ExternalID  string    `json:"external_id"`
				Status      string    `json:"status"`
				Conclusion  string    `json:"conclusion"`
				StartedAt   time.Time `json:"started_at"`
				CompletedAt time.Time `json:"completed_at"`
			}

			var (
				mu     sync.Mutex
				posted []postedReport
			)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer ghs-finishing-token" {
					t.Errorf("Authorization = %q; want credentials resolved by the finishing process", got)
				}

				if r.Method == http.MethodGet {
					if !tc.externalOnly || r.URL.Path != "/api/v3/repos/owner/infra/commits/"+selfUpdateTestCommitSHA+"/check-runs" {
						t.Errorf("unexpected check lookup: %s", r.URL.Path)
					}

					err := json.NewEncoder(w).Encode(map[string]any{
						"total_count": 2,
						"check_runs": []map[string]any{
							{"id": 71, "name": "doco-cd/nas/doco-cd", "external_id": "doco-cd:original-attempt", "app": map[string]int{"id": 12345}},
							{"id": 99, "name": "doco-cd/nas/doco-cd", "external_id": "doco-cd:newer-attempt", "app": map[string]int{"id": 12345}},
						},
					})
					if err != nil {
						t.Errorf("encode check lookup: %v", err)
					}

					return
				}

				var report postedReport
				if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
					t.Errorf("decode report: %v", err)
				}

				report.Path, report.Method = r.URL.Path, r.Method

				mu.Lock()

				posted = append(posted, report)
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			record := handedOverRecord(server.URL, selfupdate.StateFinalising)
			record.Source.CommitStatus.SourceURL = "https://github.com/owner/infra.git"

			record.Error, record.TimedOut = tc.reason, tc.timedOut
			if !tc.legacy {
				record.Source.CommitStatus.Target = &commitstatus.Target{
					Backend: commitstatus.BackendChecks, CheckRunID: 71,
					ExternalID: "doco-cd:original-attempt", AppID: "12345",
					StartedAt: record.Source.CommitStatus.StartedAt,
				}
				if tc.externalOnly {
					record.Source.CommitStatus.Target.CheckRunID = 0
				}
			}

			cfg := &app.Config{
				GitCommitStatus: tc.statusEnabled,
				GitAccessToken:  "fallback-must-not-be-used",
				GitScmProvider:  string(commitstatus.ProviderGitHub),
				GitScmApiUrl:    config.HttpUrl(server.URL),
			}
			reporter := newSelfUpdateReporter(cfg, nil)

			log := logger.New(slog.LevelError)
			if tc.failure {
				reporter.reportFailure(t.Context(), log, record)
			} else {
				reporter.reportSuccess(t.Context(), log, record)
			}

			mu.Lock()
			defer mu.Unlock()

			if len(posted) != 1 {
				t.Fatalf("posted reports = %+v; want exactly one final report", posted)
			}

			got := posted[0]

			if tc.legacy {
				wantState := commitstatus.StateSuccess
				if tc.failure {
					wantState = commitstatus.StateFailure
				}

				if got.Method != http.MethodPost || got.Path != "/api/v3/repos/owner/infra/statuses/"+selfUpdateTestCommitSHA ||
					got.State != string(wantState) || got.Context != record.Source.CommitStatus.Context {
					t.Errorf("legacy completion = %+v; want the original status, not a check", got)
				}

				return
			}

			target := record.Source.CommitStatus.Target
			if got.Method != http.MethodPatch || got.Path != "/api/v3/repos/owner/infra/check-runs/71" ||
				got.ExternalID != target.ExternalID || got.Name != record.Source.CommitStatus.Context ||
				got.Status != "completed" || got.Conclusion != tc.conclusion ||
				!got.StartedAt.Equal(target.StartedAt) || got.CompletedAt.IsZero() {
				t.Errorf("check completion = %+v; want PATCH of the original check with conclusion %s", got, tc.conclusion)
			}
		})
	}
}

func TestSelfUpdateReporterPostsFailedCommitStatus(t *testing.T) {
	t.Parallel()

	log := logger.New(slog.LevelError)

	for _, tt := range []struct {
		name          string
		statusEnabled bool
	}{
		{name: "failure with reason", statusEnabled: true},
		{name: "failure with reason after statuses disabled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := newCommitStatusRecorder(t)
			record := handedOverRecord(recorder.URL, selfupdate.StateRolledBack)
			record.Error = "successor did not become healthy:\n timeout"

			cfg := recorder.appConfig()
			cfg.GitCommitStatus = tt.statusEnabled
			newSelfUpdateReporter(cfg, nil).reportFailure(t.Context(), log, record)

			got := recorder.requireSingle(t, commitstatus.StateFailure)
			if got.Description != "successor did not become healthy: timeout" {
				t.Errorf("description = %q, want the normalized failure reason", got.Description)
			}
		})
	}

	t.Run("failure without reason", func(t *testing.T) {
		t.Parallel()

		recorder := newCommitStatusRecorder(t)
		newSelfUpdateReporter(recorder.appConfig(), nil).
			reportFailure(t.Context(), log, handedOverRecord(recorder.URL, selfupdate.StateAborted))

		got := recorder.requireSingle(t, commitstatus.StateFailure)
		if got.Description != "the new version did not become healthy" {
			t.Errorf("description = %q, want the default failure reason", got.Description)
		}
	})

	skips := []struct {
		name     string
		reporter func(*commitStatusRecorder) *selfUpdateReporter
		record   func(selfupdate.Record) selfupdate.Record
	}{
		{
			name: "no pending status recorded",
			record: func(r selfupdate.Record) selfupdate.Record {
				r.Source.CommitStatus = nil
				return r
			},
		},
		{
			name:     "nil reporter",
			reporter: func(*commitStatusRecorder) *selfUpdateReporter { return nil },
		},
	}

	for _, tt := range skips {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := newCommitStatusRecorder(t)

			reporter := newSelfUpdateReporter(recorder.appConfig(), nil)
			if tt.reporter != nil {
				reporter = tt.reporter(recorder)
			}

			record := handedOverRecord(recorder.URL, selfupdate.StateFinalising)
			if tt.record != nil {
				record = tt.record(record)
			}

			reporter.reportSuccess(t.Context(), log, record)
			reporter.reportFailure(t.Context(), log, record)

			if posted := recorder.statuses(); len(posted) != 0 {
				t.Errorf("posted %+v, want no commit status", posted)
			}
		})
	}
}

// TestSuccessorFinalisationPostsSuccessCommitStatus checks that the successor
// resolves the pending status of the deployment it took over.
func TestSuccessorFinalisationPostsSuccessCommitStatus(t *testing.T) {
	previous := docker.SelfUpdateConfig()

	docker.ConfigureSelfUpdate(docker.SelfUpdateOptions{Identity: selfupdate.Identity{ContainerID: "new"}})
	t.Cleanup(func() { docker.ConfigureSelfUpdate(previous) })

	store := selfupdate.NewStore(t.TempDir())

	var record selfupdate.Record

	recorder := newCommitStatusRecorder(t, func() {
		if _, err := store.Load(record.ID); err != nil {
			t.Errorf("journal missing before posting final status: %v", err)
		}
	})

	record = handedOverRecord(recorder.URL, selfupdate.StateDrained)
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	cfg := recorder.appConfig()
	cfg.GitCommitStatus = false

	err := finalizeAsSuccessor(t.Context(), logger.New(slog.LevelError), &finalizerDockerClient{},
		newSelfUpdateReporter(cfg, nil), store, record, "new")
	if err != nil {
		t.Fatalf("finalizeAsSuccessor() = %v", err)
	}

	recorder.requireSingle(t, commitstatus.StateSuccess)

	if _, err = store.Load(record.ID); !errors.Is(err, selfupdate.ErrNoRecord) {
		t.Errorf("record still active after finalisation: %v", err)
	}
}

// TestRolledBackPredecessorPostsFailureCommitStatus checks that the instance
// that survived a failed handover resolves the pending status.
func TestRolledBackPredecessorPostsFailureCommitStatus(t *testing.T) {
	previous := docker.SelfUpdateConfig()

	docker.ConfigureSelfUpdate(docker.SelfUpdateOptions{Identity: selfupdate.Identity{ContainerID: "old"}})
	t.Cleanup(func() { docker.ConfigureSelfUpdate(previous) })

	recorder := newCommitStatusRecorder(t)
	store := selfupdate.NewStore(t.TempDir())

	record := handedOverRecord(recorder.URL, selfupdate.StateRolledBack)
	record.Strategy = selfupdate.StrategyApplier
	record.Error = "new version unhealthy"

	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	cfg := recorder.appConfig()
	cfg.GitCommitStatus = false

	err := finalizeAsPredecessor(t.Context(), logger.New(slog.LevelError), &finalizerDockerClient{},
		newSelfUpdateReporter(cfg, nil), store, record)
	if err != nil {
		t.Fatalf("finalizeAsPredecessor() = %v", err)
	}

	got := recorder.requireSingle(t, commitstatus.StateFailure)
	if got.Description != "new version unhealthy" {
		t.Errorf("description = %q, want the rollback reason", got.Description)
	}
}

type reportingHealthClient struct {
	finalizerDockerClient
	status container.HealthStatus
}

func (c *reportingHealthClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: container.InspectResponse{
		State: &container.State{Running: true, Health: &container.Health{Status: c.status}},
	}}, nil
}

func TestPredecessorHealthFailureRecordsTimeout(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		status   container.HealthStatus
		timedOut bool
	}{
		{name: "ordinary unhealthy", status: container.Unhealthy},
		{name: "health deadline", status: container.Starting, timedOut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := selfupdate.NewStore(t.TempDir())

			var record selfupdate.Record

			recorder := newCommitStatusRecorder(t, func() {
				persisted, err := store.Load(record.ID)
				if err != nil {
					t.Errorf("load failure journal before final reporting: %v", err)
					return
				}

				if persisted.State != selfupdate.StateAborted || persisted.TimedOut != tc.timedOut {
					t.Errorf("failure journal = %+v; want aborted with timedOut=%t", persisted, tc.timedOut)
				}
			})
			record = handedOverRecord(recorder.URL, selfupdate.StateStarted)

			record.Deploy.TimeoutSeconds = 1
			if err := store.Create(&record); err != nil {
				t.Fatal(err)
			}

			err := finalizeAsPredecessor(t.Context(), logger.New(slog.LevelError), &reportingHealthClient{status: tc.status},
				newSelfUpdateReporter(recorder.appConfig(), nil), store, record)
			if err != nil {
				t.Fatal(err)
			}

			recorder.requireSingle(t, commitstatus.StateFailure)

			if _, err = store.Load(record.ID); !errors.Is(err, selfupdate.ErrNoRecord) {
				t.Errorf("journal still active after health failure reporting: %v", err)
			}
		})
	}
}
