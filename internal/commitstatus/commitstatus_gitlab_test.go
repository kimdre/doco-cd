package commitstatus_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/kimdre/doco-cd/internal/commitstatus"
)

func TestPost_ExplicitProvider_GitLabOnUnexpectedHost(t *testing.T) {
	t.Parallel()

	received := map[string]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Assert(t, strings.Contains(r.URL.Path, "/api/v4/projects/"), "unexpected path: %s", r.URL.Path)

		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderGitLab,
		srv.URL+"/owner/repo", "owner/repo", "abc123", "token",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.NilError(t, err)
	assert.Equal(t, received["state"], "success")
}

func TestPost_GitLabAPI_Failure(t *testing.T) {
	t.Parallel()

	received := map[string]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Assert(t, strings.Contains(r.URL.RawPath, "owner%2Frepo"), "unexpected raw path: %s", r.URL.RawPath)
		assert.Assert(t, strings.HasSuffix(r.URL.Path, "/deadbeef"), "unexpected path: %s", r.URL.Path)

		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := commitstatus.PostGitLab(context.Background(),
		srv.URL, "owner/repo", "deadbeef", "mytoken",
		commitstatus.Status{
			State:       commitstatus.StateFailure,
			Description: "Deploy failed",
			Context:     "ci/deploy",
		})
	assert.NilError(t, err)
	assert.Equal(t, received["state"], "failed")
	assert.Equal(t, received["description"], "Deploy failed")
}

func TestPost_GitLabStatePending(t *testing.T) {
	t.Parallel()

	received := map[string]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := commitstatus.PostGitLab(context.Background(),
		srv.URL, "owner/repo", "abc123", "token",
		commitstatus.Status{State: commitstatus.StatePending})
	assert.NilError(t, err)
	assert.Equal(t, received["state"], "running")
}

func TestPost_GitLabStateSuccess(t *testing.T) {
	t.Parallel()

	received := map[string]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := commitstatus.PostGitLab(context.Background(),
		srv.URL, "owner/repo", "abc123", "token",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.NilError(t, err)
	assert.Equal(t, received["state"], "success")
}

func TestGet_GitLabAPI(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodGet)
		assert.Assert(t, strings.Contains(r.URL.Path, "/api/v4/projects/"), "unexpected path: %s", r.URL.Path)
		assert.Assert(t, strings.HasSuffix(r.URL.Path, "/repository/commits/deadbeef/statuses"), "unexpected path: %s", r.URL.Path)

		_ = json.NewEncoder(w).Encode([]map[string]string{
			{
				"status":      "success",
				"name":        commitstatus.BaseContext,
				"description": "Successful in 47s",
				"target_url":  "https://example.com/logs/1",
			},
		})
	}))
	defer srv.Close()

	status, found, err := commitstatus.Get(context.Background(),
		commitstatus.ProviderGitLab,
		srv.URL+"/owner/repo", "owner/repo", "deadbeef", "token", commitstatus.BaseContext)
	assert.NilError(t, err)
	assert.Assert(t, found)
	assert.Equal(t, status.State, commitstatus.StateSuccess)
	assert.Equal(t, status.Description, "Successful in 47s")
}
