package commitstatus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGitLabNativeStates(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		status Status
		want   string
	}{
		{Status{State: StatePending, Outcome: OutcomeQueued}, "pending"},
		{Status{State: StatePending, Outcome: OutcomeDeferred}, "pending"},
		{Status{State: StatePending, Outcome: OutcomeInProgress}, "running"},
		{Status{State: StatePending}, "running"},
		{Status{State: StateSuccess, Outcome: OutcomeSkipped}, "skipped"},
		{Status{State: StateSuccess}, "success"},
		{Status{State: StateFailure, Outcome: OutcomeTimedOut}, "failed"},
		{Status{State: StateError, Outcome: OutcomeTimedOut}, "failed"},
		{Status{State: StateError}, "failed"},
	} {
		if got := commitStatusToGitLabState(tt.status); got != tt.want {
			t.Errorf("commitStatusToGitLabState(%+v) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestGitLabInvalidTransitions(t *testing.T) {
	t.Parallel()

	const invalidTransition = `{"message":"Cannot transition status via :enqueue from :pending (Reason(s): Status cannot transition via \"enqueue\")"}`

	for _, tt := range []struct {
		name     string
		status   Status
		reject   map[string]string
		wantErr  bool
		wantPost []string
	}{
		{
			name:     "repeated pending is already active",
			status:   Status{State: StatePending, Outcome: OutcomeQueued},
			reject:   map[string]string{"pending": invalidTransition},
			wantPost: []string{"pending"},
		},
		{
			name:     "repeated running is already active",
			status:   Status{State: StatePending},
			reject:   map[string]string{"running": invalidTransition},
			wantPost: []string{"running"},
		},
		{
			name:     "running status cannot be skipped",
			status:   Status{State: StateSuccess, Outcome: OutcomeSkipped},
			reject:   map[string]string{"skipped": invalidTransition},
			wantPost: []string{"skipped", "success"},
		},
		{
			name:     "terminal rejection is reported",
			status:   Status{State: StateFailure},
			reject:   map[string]string{"failed": invalidTransition},
			wantErr:  true,
			wantPost: []string{"failed"},
		},
		{
			name:     "other bad requests are reported",
			status:   Status{State: StatePending, Outcome: OutcomeQueued},
			reject:   map[string]string{"pending": `{"message":"State is required"}`},
			wantErr:  true,
			wantPost: []string{"pending"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var posted []string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode status: %v", err)
				}

				posted = append(posted, body["state"])

				if message, ok := tt.reject[body["state"]]; ok {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(message))

					return
				}

				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()

			err := postGitLab(t.Context(), server.URL, "owner/repo", "deadbeef", "token", tt.status)
			if (err != nil) != tt.wantErr || strings.Join(posted, ",") != strings.Join(tt.wantPost, ",") {
				t.Fatalf("postGitLab() err=%v posted=%v; want err=%t posted=%v", err, posted, tt.wantErr, tt.wantPost)
			}
		})
	}
}

func TestGitLabReadsNativeStates(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		state   string
		want    State
		outcome Outcome
	}{
		{"pending", StatePending, OutcomeQueued},
		{"running", StatePending, OutcomeInProgress},
		{"skipped", StateSuccess, OutcomeSkipped},
		{"success", StateSuccess, ""},
		{"failed", StateFailure, ""},
		{"canceled", StateError, ""},
	} {
		if state, outcome := gitLabStateToCommitStatus(tt.state); state != tt.want || outcome != tt.outcome {
			t.Errorf("gitLabStateToCommitStatus(%q) = %q/%q, want %q/%q", tt.state, state, outcome, tt.want, tt.outcome)
		}
	}
}

func TestGiteaVersionSupportsSkipped(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		version string
		want    bool
	}{
		{"1.25.0", true},
		{"v1.25.0-rc0", true},
		{"1.27.0+dev-955-g37488799e1", true},
		{"2.0.0", true},
		{"1.24.6", false},
		{"1.21.11-1", false},
		{"16.0.5+gitea-1.22.0", true},
		{"16.0.0-dev-753-6bcc6da0+gitea-1.22.0", true},
		{"15.0.1+gitea-1.22.0", false},
		{"", false},
		{"unknown", false},
	} {
		if got := giteaVersionSupportsSkipped(tt.version); got != tt.want {
			t.Errorf("giteaVersionSupportsSkipped(%q) = %t, want %t", tt.version, got, tt.want)
		}
	}
}

func TestGiteaSkippedRequiresSupportedServer(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		version string
		status  Status
		want    string
		lookups int32
	}{
		{"gitea 1.25", "1.25.0", Status{State: StateSuccess, Outcome: OutcomeSkipped}, "skipped", 1},
		{"forgejo 16", "16.0.5+gitea-1.22.0", Status{State: StateSuccess, Outcome: OutcomeSkipped}, "skipped", 1},
		{"gitea 1.24", "1.24.6", Status{State: StateSuccess, Outcome: OutcomeSkipped}, "success", 1},
		{"forgejo 15", "15.0.1+gitea-1.22.0", Status{State: StateSuccess, Outcome: OutcomeSkipped}, "success", 1},
		{"version unavailable", "", Status{State: StateSuccess, Outcome: OutcomeSkipped}, "success", 1},
		{"not skipped", "1.25.0", Status{State: StateSuccess}, "success", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				lookups atomic.Int32
				posted  []string
			)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/version" {
					lookups.Add(1)

					if tt.version == "" {
						w.WriteHeader(http.StatusNotFound)

						return
					}

					_ = json.NewEncoder(w).Encode(map[string]string{"version": tt.version})

					return
				}

				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode status: %v", err)
				}

				posted = append(posted, body["state"])

				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()

			for range 2 {
				if err := postGitHubCompatible(t.Context(), server.URL, "", "owner/repo", "deadbeef", "token", tt.status); err != nil {
					t.Fatal(err)
				}
			}

			wantLookups := tt.lookups
			if tt.version == "" {
				// Failed lookups are not cached, so each skipped status retries detection.
				wantLookups = 2
			}

			if posted[0] != tt.want || posted[1] != tt.want || lookups.Load() != wantLookups {
				t.Fatalf("posted=%v lookups=%d; want %q with %d lookups", posted, lookups.Load(), tt.want, wantLookups)
			}
		})
	}
}

func TestGitHubStyleReadsGiteaSkipped(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"state":"skipped","context":"doco-cd/deploy","description":"Skipped"}]`))
	}))
	defer server.Close()

	status, found, err := getGitHubCompatible(t.Context(), server.URL, "owner/repo", "deadbeef", "token", DeployContext)
	if err != nil || !found || status.State != StateSuccess || status.Outcome != OutcomeSkipped {
		t.Fatalf("getGitHubCompatible() = %+v found=%t err=%v", status, found, err)
	}
}

func TestAzureDevOpsSkippedIsNotApplicable(t *testing.T) {
	t.Parallel()

	if got := commitStatusToAzureState(Status{State: StateSuccess, Outcome: OutcomeSkipped}); got != "notApplicable" {
		t.Fatalf("skipped Azure DevOps state = %q, want notApplicable", got)
	}

	if got := commitStatusToAzureState(Status{State: StateSuccess}); got != "succeeded" {
		t.Fatalf("success Azure DevOps state = %q, want succeeded", got)
	}

	if state, outcome := azureStateToCommitStatus("notApplicable"); state != StateSuccess || outcome != OutcomeSkipped {
		t.Fatalf("notApplicable read as %q/%q, want success/skipped", state, outcome)
	}
}
