package stages

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
)

const testPhaseDwell = 20 * time.Millisecond

type recordedPhases struct {
	mu     sync.Mutex
	phases []string
}

func (r *recordedPhases) post(_ context.Context, phase string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.phases = append(r.phases, phase)

	return nil
}

func (r *recordedPhases) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.phases)
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}

		time.Sleep(5 * time.Millisecond)
	}
}

func TestPhaseReporterPublishesLatestPhaseAfterDwell(t *testing.T) {
	t.Parallel()

	// A longer dwell keeps the fast phases below it even on a slow machine.
	const dwell = 10 * testPhaseDwell

	recorded := &recordedPhases{}
	reporter := newPhaseReporter(t.Context(), newTestStageManager(t).Log, dwell, recorded.post)

	defer reporter.Stop()

	// Fast phases are replaced before the dwell time passes.
	reporter.Report("resolving working directory")
	reporter.Report("loading compose configuration")
	reporter.Report("pulling images")

	waitFor(t, func() bool { return len(recorded.snapshot()) == 1 })

	// A repeated phase is not published again.
	reporter.Report("pulling images")
	time.Sleep(2 * dwell)

	reporter.Report("waiting for services to start")
	waitFor(t, func() bool { return len(recorded.snapshot()) == 2 })

	want := []string{"pulling images", "waiting for services to start"}
	if got := recorded.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("published phases = %q, want %q", got, want)
	}
}

func TestPhaseReporterNewPhaseDoesNotInheritExpiredDwell(t *testing.T) {
	t.Parallel()

	const dwell = 100 * time.Millisecond

	type phaseUpdate struct {
		phase string
		at    time.Time
	}

	updates := make(chan phaseUpdate, 2)
	reporter := newPhaseReporter(t.Context(), newTestStageManager(t).Log, dwell,
		func(_ context.Context, phase string) error {
			updates <- phaseUpdate{phase: phase, at: time.Now()}

			return nil
		})

	defer reporter.Stop()

	reporter.Report("pulling images")
	waitFor(t, func() bool { return len(reporter.notify) == 0 })

	// Hold the snapshot lock until the old phase's timer expires, with
	// the replacement report already waiting to update the snapshot.
	reporter.mu.Lock()

	started := make(chan struct{})
	reported := make(chan struct{})

	go func() {
		close(started)
		reporter.Report("creating services")
		close(reported)
	}()

	<-started
	time.Sleep(2 * dwell)

	changedAt := time.Now()
	reporter.mu.Unlock()
	<-reported

	for {
		select {
		case update := <-updates:
			if update.phase != "creating services" {
				continue
			}

			if elapsed := update.at.Sub(changedAt); elapsed < dwell {
				t.Fatalf("replacement phase published after %s, want at least %s", elapsed, dwell)
			}

			return
		case <-time.After(2 * time.Second):
			t.Fatal("replacement phase was not published")
		}
	}
}

func TestPhaseReporterStopWaitsForPostInFlight(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	recorded := &recordedPhases{}

	reporter := newPhaseReporter(t.Context(), newTestStageManager(t).Log, testPhaseDwell,
		func(ctx context.Context, phase string) error {
			if len(recorded.snapshot()) == 0 {
				close(started)
				<-release
			}

			return recorded.post(ctx, phase)
		})

	reporter.Report("pulling images")
	<-started

	stopped := make(chan struct{})

	go func() {
		reporter.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned while a phase update was in flight")
	case <-time.After(3 * testPhaseDwell):
	}

	close(release)
	<-stopped

	// Reports after Stop are ignored, and Stop is idempotent.
	reporter.Report("creating services")
	time.Sleep(3 * testPhaseDwell)
	reporter.Stop()

	if got := recorded.snapshot(); !slices.Equal(got, []string{"pulling images"}) {
		t.Fatalf("published phases = %q, want only the in-flight phase", got)
	}
}

func TestPhaseReporterContinuesAfterOrdinaryFailure(t *testing.T) {
	t.Parallel()

	recorded := &recordedPhases{}
	failed := false

	reporter := newPhaseReporter(t.Context(), newTestStageManager(t).Log, testPhaseDwell,
		func(ctx context.Context, phase string) error {
			if !failed {
				failed = true

				return errors.New("bad gateway")
			}

			return recorded.post(ctx, phase)
		})

	defer reporter.Stop()

	reporter.Report("pulling images")
	time.Sleep(3 * testPhaseDwell)
	reporter.Report("creating services")

	waitFor(t, func() bool { return len(recorded.snapshot()) == 1 })

	if got := recorded.snapshot(); !slices.Equal(got, []string{"creating services"}) {
		t.Fatalf("published phases = %q, want creating services", got)
	}
}

// checkPhaseServer records the titles of check run updates and answers with status.
type checkPhaseServer struct {
	mu     sync.Mutex
	titles []string
	status int
}

func (s *checkPhaseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Output struct {
			Title string `json:"title"`
		} `json:"output"`
	}

	if r.Method != http.MethodPatch {
		http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)

		return
	}

	_ = json.NewDecoder(r.Body).Decode(&body)

	s.mu.Lock()
	s.titles = append(s.titles, body.Output.Title)
	status := s.status
	s.mu.Unlock()

	if status != 0 {
		http.Error(w, `{"message":"You have exceeded a secondary rate limit"}`, status)
	}
}

func (s *checkPhaseServer) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.titles)
}

func newPhaseTestManager(t *testing.T, server *httptest.Server) *StageManager {
	t.Helper()

	sm := newTestStageManager(t)
	sm.AppConfig = &app.Config{
		GitCommitStatus: true, GitScmProvider: "github", GitScmApiUrl: config.HttpUrl(server.URL), GitAccessToken: "test-token",
	}
	sm.Repository.SourceUrl = "https://github.com/owner/repo"
	sm.Repository.Revision = "deadbeef"
	sm.commitStatusTarget = &commitstatus.Target{Backend: commitstatus.BackendChecks, CheckRunID: 123, ExternalID: "attempt"}
	sm.inProgressPosted = true

	return sm
}

func TestStartPhaseReporterUpdatesCheckRun(t *testing.T) {
	t.Parallel()

	checks := &checkPhaseServer{}
	server := httptest.NewServer(checks)
	t.Cleanup(server.Close)

	sm := newPhaseTestManager(t, server)

	reporter := sm.startPhaseReporter(t.Context(), testPhaseDwell)
	if reporter == nil {
		t.Fatal("startPhaseReporter() = nil, want a reporter")
	}

	reporter.Report("pulling images")
	waitFor(t, func() bool { return len(checks.snapshot()) == 1 })
	reporter.Stop()

	if got := checks.snapshot(); got[0] != "Deploying: pulling images" {
		t.Fatalf("check title = %q, want %q", got[0], "Deploying: pulling images")
	}
}

func TestStartPhaseReporterStopsOnRateLimit(t *testing.T) {
	t.Parallel()

	checks := &checkPhaseServer{status: http.StatusForbidden}
	server := httptest.NewServer(checks)
	t.Cleanup(server.Close)

	reporter := newPhaseTestManager(t, server).startPhaseReporter(t.Context(), testPhaseDwell)

	defer reporter.Stop()

	reporter.Report("pulling images")
	waitFor(t, func() bool { return len(checks.snapshot()) == 1 })

	reporter.Report("creating services")
	time.Sleep(5 * testPhaseDwell)

	// A single attempt, and no further updates once rate limited.
	if got := checks.snapshot(); len(got) != 1 {
		t.Fatalf("check updates = %q, want exactly one", got)
	}
}

func TestStartPhaseReporterSkipsUnsupportedDeployments(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)

	for _, tt := range []struct {
		name  string
		setup func(sm *StageManager)
	}{
		{"in progress not posted", func(sm *StageManager) { sm.inProgressPosted = false }},
		{"destroy", func(sm *StageManager) { sm.DeployConfig.Destroy.Enabled = true }},
		{"commit status disabled", func(sm *StageManager) { sm.AppConfig.GitCommitStatus = false }},
		{"gitlab", func(sm *StageManager) {
			sm.AppConfig.GitScmProvider = "gitlab"
			sm.Repository.SourceUrl = "https://gitlab.com/owner/repo"
			sm.commitStatusTarget = &commitstatus.Target{Backend: commitstatus.BackendStatus}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sm := newPhaseTestManager(t, server)
			tt.setup(sm)

			if reporter := sm.startPhaseReporter(t.Context(), testPhaseDwell); reporter != nil {
				reporter.Stop()
				t.Fatal("startPhaseReporter() returned a reporter, want nil")
			}
		})
	}
}
