package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestEarlyFailureCommitStatusDescription(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "Failed",
		},
		{
			name: "normalizes whitespace",
			err:  assertErr("yaml:\n line 24: could not find expected ':'"),
			want: "yaml: line 24: could not find expected ':'",
		},
		{
			name: "truncates long error",
			err:  assertErr(strings.Repeat("x", 200)),
			want: strings.Repeat("x", 137) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := earlyFailureCommitStatusDescription(tt.err)
			if got != tt.want {
				t.Fatalf("earlyFailureCommitStatusDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPostEarlyCommitStatus(t *testing.T) {
	var received map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/repos/owner/repo/statuses/deadbeef"; got != want {
			t.Fatalf("unexpected path: got %q want %q", got, want)
		}

		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	postEarlyCommitStatus(context.Background(), slog.Default(), &app.Config{
		GitCommitStatus: true,
		GitAccessToken:  "token",
		GitScmProvider:  "gitea",
	}, config.SourceTypeGit, srv.URL+"/owner/repo", "deadbeef", webhook.ParsedPayload{
		FullName: "owner/repo",
	}, "bad config")

	if got, want := received["state"], "error"; got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}

	if got, want := received["description"], "bad config"; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
}

func assertErr(msg string) error {
	return simpleError(msg)
}

type simpleError string

func (e simpleError) Error() string {
	return string(e)
}
