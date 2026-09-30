package commitstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/common/lifecycle"
)

func checkTestRequest(baseURL string) Request {
	return Request{
		Provider:     ProviderGitHub,
		APIBaseURL:   baseURL,
		RepoURL:      "https://github.com/owner/repo",
		RepoFullName: "owner/repo",
		CommitSHA:    "deadbeef",
		Token:        "installation-token",
		Context:      "doco-cd/target/stack",
		Target:       &Target{Backend: BackendChecks, ExternalID: "doco-cd:attempt", AppID: "42"},
	}
}

func TestGitHubCheckLockHonorsCancellation(t *testing.T) {
	t.Parallel()

	unlock, err := lockGitHubCheck(t.Context(), t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := lockGitHubCheck(ctx, t.Name()); !errors.Is(err, context.Canceled) {
		t.Fatalf("lock ignored cancellation: %v", err)
	}
}

func TestGitHubCheckLifecycle(t *testing.T) {
	t.Parallel()

	var received []githubCheckRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer installation-token" {
			t.Errorf("unexpected authorization: %q", r.Header.Get("Authorization"))
		}

		if r.Method == http.MethodGet {
			if r.URL.Query().Get("check_name") != "doco-cd/target/stack" ||
				r.URL.Path != "/repos/owner/repo/commits/deadbeef/check-runs" {
				t.Errorf("unexpected check lookup: %s", r.URL)
			}

			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))

			return
		}

		var body githubCheckRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode check: %v", err)
		}

		received = append(received, body)

		if len(received) == 1 {
			if r.Method != http.MethodPost || r.URL.Path != "/repos/owner/repo/check-runs" {
				t.Errorf("unexpected creation: %s %s", r.Method, r.URL)
			}

			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":123}`))
		} else if r.Method != http.MethodPatch || r.URL.Path != "/repos/owner/repo/check-runs/123" {
			t.Errorf("unexpected update: %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()

	req := checkTestRequest(server.URL)
	for _, status := range []Status{
		{State: StatePending, Outcome: OutcomeQueued, Description: "Queued"},
		{State: StatePending, Description: "In Progress"},
		{State: StateSuccess, Description: "Successful in 3s", TargetURL: "https://example.com/logs"},
	} {
		if err := req.Post(t.Context(), status); err != nil {
			t.Fatal(err)
		}
	}

	if req.Target.CheckRunID != 123 || len(received) != 3 {
		t.Fatalf("target=%+v, received=%+v", req.Target, received)
	}

	if received[0].Status != "queued" || received[0].Conclusion != "" ||
		received[0].HeadSHA != "deadbeef" || received[0].StartedAt != nil {
		t.Fatalf("unexpected queued check: %+v", received[0])
	}

	if received[1].Status != "in_progress" || received[1].Conclusion != "" ||
		received[1].HeadSHA != "" || received[1].StartedAt == nil {
		t.Fatalf("unexpected running check: %+v", received[1])
	}

	if received[2].Status != "completed" || received[2].Conclusion != "success" ||
		received[2].CompletedAt == nil || !received[2].StartedAt.Equal(*received[1].StartedAt) ||
		received[2].DetailsURL != "https://example.com/logs" {
		t.Fatalf("unexpected successful check: %+v", received[2])
	}

	for _, body := range received {
		if body.Name != req.Context || body.ExternalID != "doco-cd:attempt" ||
			body.Output.Title == "" || body.Output.Summary != body.Output.Title {
			t.Fatalf("unexpected check metadata: %+v", body)
		}
	}
}

func TestGitHubCheckConclusions(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		state      State
		outcome    Outcome
		conclusion string
	}{
		{StateSuccess, OutcomeSkipped, "skipped"},
		{StateFailure, OutcomeTimedOut, "timed_out"},
		{StateError, OutcomeTimedOut, "timed_out"},
		{StateFailure, "", "failure"},
		{StateError, "", "failure"},
	} {
		t.Run(string(tt.state)+"/"+string(tt.outcome), func(t *testing.T) {
			t.Parallel()

			req := checkTestRequest("")

			body, err := req.checkRequest(Status{State: tt.state, Outcome: tt.outcome, Description: "Result"})
			if err != nil {
				t.Fatal(err)
			}

			if body.Status != "completed" || body.Conclusion != tt.conclusion || body.CompletedAt == nil {
				t.Fatalf("unexpected conclusion: %+v", body)
			}
		})
	}
}

func TestGitHubCheckFallsBackToOutcomeTitle(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		status Status
		title  string
	}{
		{Status{State: StateFailure, Outcome: OutcomeTimedOut}, "Timed out"},
		{Status{State: StateSuccess, Outcome: OutcomeSkipped, Description: " "}, "Skipped"},
		{Status{State: StatePending, Outcome: OutcomeQueued}, "Queued"},
		{Status{State: StatePending}, "In progress"},
	} {
		body, err := checkTestRequest("").checkRequest(tt.status)
		if err != nil {
			t.Fatal(err)
		}

		if body.Output.Title != tt.title || body.Output.Summary != tt.title {
			t.Fatalf("checkRequest(%+v) output = %+v, want %q", tt.status, body.Output, tt.title)
		}
	}
}

func TestLegacyStatusesPreserveNativeOutcomeFallbacks(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		state   State
		outcome Outcome
	}{
		{StatePending, OutcomeQueued},
		{StatePending, OutcomeDeferred},
		{StateSuccess, OutcomeSkipped},
		{StateError, OutcomeTimedOut},
		{StateFailure, OutcomeTimedOut},
	} {
		t.Run(string(tt.state)+"/"+string(tt.outcome), func(t *testing.T) {
			t.Parallel()

			var body map[string]string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/statuses/deadbeef") {
					t.Errorf("unexpected legacy request: %s %s", r.Method, r.URL)
				}

				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode status: %v", err)
				}

				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()

			req := checkTestRequest("")
			req.RepoURL = server.URL + "/owner/repo"
			req.Target = &Target{Backend: BackendStatus}

			err := req.Post(t.Context(), Status{State: tt.state, Outcome: tt.outcome, Description: "Legacy result"})
			if err != nil || body["state"] != string(tt.state) || body["description"] != "Legacy result" {
				t.Fatalf("legacy mapping changed: body=%v err=%v", body, err)
			}
		})
	}
}

func TestFailureOutcomeUsesTypedDeadline(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		err  error
		want Outcome
	}{
		{context.DeadlineExceeded, OutcomeTimedOut},
		{fmt.Errorf("waiting: %w", context.DeadlineExceeded), OutcomeTimedOut},
		{fmt.Errorf("readiness: %w", lifecycle.ErrTimedOut), OutcomeTimedOut},
		{context.Canceled, ""},
		{errors.New("timeout-like error message"), ""},
		{nil, ""},
	} {
		if got := FailureOutcome(tt.err); got != tt.want {
			t.Fatalf("FailureOutcome(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestGitHubCheckResumesDeferredAttempt(t *testing.T) {
	t.Parallel()

	var saved githubCheck

	var lastBody githubCheckRequest

	queuedAt := time.Now().Add(-time.Hour).UTC()
	creates := 0
	updates := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			checks := []githubCheck{}
			if saved.ID != 0 {
				checks = append(checks, saved)
			}

			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(checks), "check_runs": checks})
		case http.MethodPost, http.MethodPatch:
			var body githubCheckRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode check: %v", err)
			}

			lastBody = body

			if r.Method == http.MethodPost {
				creates++
			} else {
				updates++

				if !strings.HasSuffix(r.URL.Path, "/456") {
					t.Errorf("updated wrong check: %s", r.URL)
				}
			}

			saved = githubCheck{ID: 456, Name: body.Name, ExternalID: body.ExternalID, Status: body.Status, StartedAt: &queuedAt}
			saved.App.ID = 42
			_ = json.NewEncoder(w).Encode(saved)
		}
	}))
	defer server.Close()

	original := checkTestRequest(server.URL)
	if err := original.Post(t.Context(), Status{
		State: StatePending, Outcome: OutcomeDeferred, Description: "Deferred by sync window",
	}); err != nil {
		t.Fatal(err)
	}

	resumed := checkTestRequest(server.URL)

	resumed.Target.ExternalID = "doco-cd:later-attempt"
	if err := resumed.Post(t.Context(), Status{State: StatePending, Outcome: OutcomeQueued, Description: "Queued"}); err != nil {
		t.Fatal(err)
	}

	if creates != 1 || updates != 1 || resumed.Target.CheckRunID != 456 ||
		saved.ExternalID != "doco-cd:later-attempt" || !resumed.Target.StartedAt.IsZero() {
		t.Fatalf("deferred target not claimed: creates=%d updates=%d target=%+v saved=%+v",
			creates, updates, resumed.Target, saved)
	}

	if err := resumed.Post(t.Context(), Status{State: StatePending, Description: "Deploying"}); err != nil {
		t.Fatal(err)
	}

	if lastBody.StartedAt == nil || !lastBody.StartedAt.After(queuedAt) {
		t.Fatalf("resumed check must start when admitted, got started_at=%v", lastBody.StartedAt)
	}

	if err := resumed.Post(t.Context(), Status{State: StateSuccess, Description: "Successful"}); err != nil {
		t.Fatal(err)
	}

	if updates != 3 || saved.Status != "completed" {
		t.Fatalf("deferred check not finished: updates=%d saved=%+v", updates, saved)
	}
}

func TestGitHubDeferredChecksAreScopedToDockerContext(t *testing.T) {
	t.Parallel()

	req := checkTestRequest("")
	req.Target.Scope = "remote-a"
	existing := githubCheck{
		ID: 999, Name: req.Context, ExternalID: req.deferredExternalID(), Status: "queued",
	}
	existing.App.ID = 42

	creates := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []githubCheck{existing}})
		case http.MethodPost:
			creates++
			_, _ = w.Write([]byte(`{"id":123}`))
		default:
			t.Errorf("updated check on another Docker context: %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()

	req.APIBaseURL = server.URL

	req.Target.Scope = "remote-b"
	if err := req.Post(t.Context(), Status{State: StatePending, Outcome: OutcomeDeferred}); err != nil {
		t.Fatal(err)
	}

	if creates != 1 || req.Target.CheckRunID != 123 {
		t.Fatalf("deferred contexts were conflated: creates=%d target=%+v", creates, req.Target)
	}
}

func TestGitHubCheckDoesNotReuseUnrelatedAttempt(t *testing.T) {
	t.Parallel()

	created := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":999,"name":"doco-cd/target/stack","external_id":"doco-cd:another-attempt","status":"queued","app":{"id":42}}]}`))
		case http.MethodPost:
			created = true
			_, _ = w.Write([]byte(`{"id":123}`))
		default:
			t.Errorf("unexpected update to unrelated attempt: %s", r.URL)
		}
	}))
	defer server.Close()

	req := checkTestRequest(server.URL)
	if err := req.Post(t.Context(), Status{State: StatePending, Outcome: OutcomeQueued}); err != nil {
		t.Fatal(err)
	}

	if !created || req.Target.CheckRunID != 123 {
		t.Fatalf("new attempt not created: %+v", req.Target)
	}
}

func TestGitHubCheckCreationRecoversAmbiguousResponse(t *testing.T) {
	t.Parallel()

	creates, patches := 0, 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if creates == 0 {
				_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
			} else {
				_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":123,"name":"doco-cd/target/stack","external_id":"doco-cd:attempt","app":{"id":42}}]}`))
			}
		case http.MethodPost:
			creates++

			w.WriteHeader(http.StatusBadGateway)
		case http.MethodPatch:
			patches++
		}
	}))
	defer server.Close()

	req := checkTestRequest(server.URL)
	if err := req.Post(t.Context(), Status{State: StatePending, Outcome: OutcomeQueued}); err != nil {
		t.Fatal(err)
	}

	if creates != 1 || patches != 1 || req.Target.CheckRunID != 123 {
		t.Fatalf("creation duplicated: creates=%d patches=%d target=%+v", creates, patches, req.Target)
	}
}

func TestGitHubCheckPermissionFailureDoesNotFallBack(t *testing.T) {
	t.Parallel()

	writes := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
			return
		}

		writes++

		if strings.Contains(r.URL.Path, "/statuses/") {
			t.Error("silently fell back to commit statuses")
		}

		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer server.Close()

	req := checkTestRequest(server.URL)

	err := req.Post(t.Context(), Status{State: StateSuccess})
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "integration") || writes != 1 {
		t.Fatalf("unexpected permission handling: writes=%d err=%v", writes, err)
	}
}

func TestGitHubCheckReadPaginatesAndFiltersApp(t *testing.T) {
	t.Parallel()

	pages := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++

		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(`{"total_count":101,"check_runs":[{"id":999,"name":"doco-cd/target/stack","status":"completed","conclusion":"success","app":{"id":43}}]}`))
		} else {
			_, _ = w.Write([]byte(`{"total_count":101,"check_runs":[{"id":123,"name":"doco-cd/target/stack","status":"completed","conclusion":"skipped","app":{"id":42},"output":{"title":"Skipped"},"details_url":"https://example.com/logs"}]}`))
		}
	}))
	defer server.Close()

	req := checkTestRequest(server.URL)

	status, found, err := req.Get(t.Context())
	if err != nil || !found || pages != 2 || status.State != StateSuccess ||
		status.Outcome != OutcomeSkipped || status.Description != "Skipped" || status.TargetURL == "" {
		t.Fatalf("unexpected check result: %+v found=%t pages=%d err=%v", status, found, pages, err)
	}
}

func TestGitHubCheckMatchesAppClientID(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[` +
			`{"id":999,"name":"doco-cd/target/stack","status":"completed","conclusion":"failure","app":{"id":43,"client_id":"Iv1.other"}},` +
			`{"id":123,"name":"doco-cd/target/stack","status":"completed","conclusion":"skipped","app":{"id":42,"client_id":"Iv1.doco"}}]}`))
	}))
	defer server.Close()

	req := checkTestRequest(server.URL)
	req.Target.AppID = "Iv1.doco"

	status, found, err := req.Get(t.Context())
	if err != nil || !found || status.Outcome != OutcomeSkipped {
		t.Fatalf("client ID did not select the app's check: %+v found=%t err=%v", status, found, err)
	}
}

func TestGitHubCheckReadsNativeOutcomes(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		status     string
		conclusion string
		state      State
		outcome    Outcome
	}{
		{"queued", "", StatePending, OutcomeQueued},
		{"in_progress", "", StatePending, OutcomeInProgress},
		{"completed", "success", StateSuccess, ""},
		{"completed", "skipped", StateSuccess, OutcomeSkipped},
		{"completed", "failure", StateFailure, ""},
		{"completed", "timed_out", StateFailure, OutcomeTimedOut},
	} {
		t.Run(tt.status+"/"+tt.conclusion, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				check := githubCheck{ID: 123, Name: "doco-cd/target/stack", Status: tt.status, Conclusion: tt.conclusion}
				check.App.ID = 42
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []githubCheck{check}})
			}))
			defer server.Close()

			req := checkTestRequest(server.URL)

			got, found, err := req.Get(t.Context())
			if err != nil || !found || got.State != tt.state || got.Outcome != tt.outcome {
				t.Fatalf("unexpected native state: %+v found=%t err=%v", got, found, err)
			}
		})
	}
}

func TestGitHubCheckAPIURLs(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		repoURL  string
		override string
		want     string
	}{
		{"https://github.com/owner/repo", "", "https://api.github.com"},
		{"https://github.com/owner/repo", "https://proxy.example/api/v3/", "https://proxy.example/api/v3"},
		{"https://ghe.example/owner/repo", "", "https://ghe.example/api/v3"},
		{"ssh://git@ghe.example:2222/owner/repo", "", "https://ghe.example/api/v3"},
		{"https://ghe.example/owner/repo", "https://ghe.example", "https://ghe.example/api/v3"},
		{"https://ghe.example/owner/repo", "https://ghe.example/api/v3", "https://ghe.example/api/v3"},
	} {
		t.Run(tt.repoURL+"/"+tt.override, func(t *testing.T) {
			req := checkTestRequest(tt.override)
			req.RepoURL = tt.repoURL

			got, err := req.githubAPIURL()
			if err != nil || got != tt.want {
				t.Fatalf("URL=%q want=%q err=%v", got, tt.want, err)
			}
		})
	}
}

func TestGitHubTimedOutCheckUsesLiveReportingContext(t *testing.T) {
	t.Parallel()

	posted := false

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			posted = true
		} else {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	req := checkTestRequest(server.URL)
	req.Target.CheckRunID = 123

	err := req.Post(ctx, Status{State: StateFailure, Outcome: FailureOutcome(fmt.Errorf("wait: %w", context.DeadlineExceeded))})
	if err != nil || !posted {
		t.Fatalf("timeout not reported: posted=%t err=%v", posted, err)
	}

	canceled, stop := context.WithCancel(t.Context())
	stop()

	err = req.Post(canceled, Status{State: StateFailure, Outcome: OutcomeTimedOut})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown cancellation not preserved: %v", err)
	}
}
