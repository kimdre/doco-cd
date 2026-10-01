package commitstatus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProgressStatus(t *testing.T) {
	t.Parallel()

	status := ProgressStatus(" pulling\n images ")
	if status.State != StatePending || status.Outcome != OutcomeInProgress {
		t.Fatalf("ProgressStatus() state = %q/%q, want pending/in_progress", status.State, status.Outcome)
	}

	if status.Description != "In Progress: pulling images" || status.Phase != "pulling images" {
		t.Fatalf("ProgressStatus() = %+v", status)
	}

	long := ProgressStatus(strings.Repeat("x", 200))
	if got := len([]rune(long.Description)); got != maxDescriptionLength {
		t.Fatalf("description length = %d, want %d", got, maxDescriptionLength)
	}

	if !strings.HasSuffix(long.Description, "...") {
		t.Fatalf("description = %q, want a truncated description", long.Description)
	}
}

func TestRequestSupportsProgress(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		req  Request
		want bool
	}{
		{"github checks", checkTestRequest(""), true},
		{"github status", Request{RepoURL: "https://github.com/owner/repo"}, true},
		{"gitea", Request{RepoURL: "https://gitea.example.com/owner/repo", Provider: ProviderGitea}, true},
		{"azure devops", Request{RepoURL: "https://dev.azure.com/org/project/_git/repo"}, true},
		{"gitlab auto-detected", Request{RepoURL: "https://gitlab.com/owner/repo"}, false},
		{"gitlab override", Request{RepoURL: "https://git.example.com/owner/repo", Provider: ProviderGitLab}, false},
		{"invalid url", Request{RepoURL: "://"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.req.SupportsProgress(); got != tt.want {
				t.Fatalf("SupportsProgress() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsRateLimited(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"too many requests", &commitStatusPostRetryError{err: errors.New("429"), statusCode: http.StatusTooManyRequests}, true},
		{"secondary rate limit", &commitStatusPostRetryError{
			err: errors.New("403"), statusCode: http.StatusForbidden,
			details: ": You have exceeded a secondary rate limit",
		}, true},
		{"primary rate limit", &commitStatusPostRetryError{
			err: errors.New("403"), statusCode: http.StatusForbidden, details: ": API rate limit exceeded",
		}, true},
		{"forbidden", &commitStatusPostRetryError{
			err: errors.New("403"), statusCode: http.StatusForbidden, details: ": Resource not accessible by integration",
		}, false},
		{"server error", &commitStatusPostRetryError{err: errors.New("502"), statusCode: http.StatusBadGateway}, false},
		{"other error", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := IsRateLimited(tt.err); got != tt.want {
				t.Fatalf("IsRateLimited() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWithSingleAttemptSkipsRetries(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)

	for _, tt := range []struct {
		name string
		ctx  context.Context
		want int32
	}{
		{"single attempt", WithSingleAttempt(t.Context()), 1},
		{"default retries", t.Context(), postRetryMaxAttempts},
	} {
		attempts.Store(0)

		err := doPost(tt.ctx, server.URL, "Bearer token", map[string]string{})
		if !IsRateLimited(err) {
			t.Fatalf("%s: doPost() error = %v, want a rate limit error", tt.name, err)
		}

		if got := attempts.Load(); got != tt.want {
			t.Fatalf("%s: attempts = %d, want %d", tt.name, got, tt.want)
		}
	}
}
