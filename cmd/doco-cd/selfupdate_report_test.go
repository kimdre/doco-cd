package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker"
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

func newCommitStatusRecorder(t *testing.T) *commitStatusRecorder {
	t.Helper()

	recorder := &commitStatusRecorder{}
	recorder.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var status postedCommitStatus
		if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
			t.Errorf("decode status: %v", err)
		}

		status.Path = r.URL.Path

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

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		recorder := newCommitStatusRecorder(t)
		newSelfUpdateReporter(recorder.appConfig(), nil).
			reportSuccess(t.Context(), log, handedOverRecord(recorder.URL, selfupdate.StateFinalising))

		got := recorder.requireSingle(t, commitstatus.StateSuccess)
		if !strings.HasPrefix(got.Description, "Successful in 1m") {
			t.Errorf("description = %q, want the duration since the deployment started", got.Description)
		}
	})

	t.Run("failure with reason", func(t *testing.T) {
		t.Parallel()

		recorder := newCommitStatusRecorder(t)
		record := handedOverRecord(recorder.URL, selfupdate.StateRolledBack)
		record.Error = "successor did not become healthy:\n timeout"

		newSelfUpdateReporter(recorder.appConfig(), nil).reportFailure(t.Context(), log, record)

		got := recorder.requireSingle(t, commitstatus.StateFailure)
		if got.Description != "successor did not become healthy: timeout" {
			t.Errorf("description = %q, want the normalized failure reason", got.Description)
		}
	})

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
			name: "commit statuses disabled",
			reporter: func(rec *commitStatusRecorder) *selfUpdateReporter {
				cfg := rec.appConfig()
				cfg.GitCommitStatus = false

				return newSelfUpdateReporter(cfg, nil)
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

	recorder := newCommitStatusRecorder(t)
	store := selfupdate.NewStore(t.TempDir())

	record := handedOverRecord(recorder.URL, selfupdate.StateDrained)
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	err := finalizeAsSuccessor(t.Context(), logger.New(slog.LevelError), &finalizerDockerClient{},
		newSelfUpdateReporter(recorder.appConfig(), nil), store, record, "new")
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

	err := finalizeAsPredecessor(t.Context(), logger.New(slog.LevelError), &finalizerDockerClient{},
		newSelfUpdateReporter(recorder.appConfig(), nil), store, record)
	if err != nil {
		t.Fatalf("finalizeAsPredecessor() = %v", err)
	}

	got := recorder.requireSingle(t, commitstatus.StateFailure)
	if got.Description != "new version unhealthy" {
		t.Errorf("description = %q, want the rollback reason", got.Description)
	}
}
