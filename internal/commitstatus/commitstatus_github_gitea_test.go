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

func TestPost_GiteaAPI(t *testing.T) {
	t.Parallel()

	received := map[string]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Assert(t, strings.Contains(r.URL.Path, "/api/v1/repos/"), "unexpected path: %s", r.URL.Path)
		assert.Assert(t, strings.HasSuffix(r.URL.Path, "/statuses/deadbeef"), "unexpected path: %s", r.URL.Path)
		assert.Equal(t, r.Header.Get("Authorization"), "token mytoken")
		assert.Equal(t, r.Header.Get("Content-Type"), "application/json")

		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	host, scheme, err := commitstatus.ParseHostAndScheme(srv.URL)
	assert.NilError(t, err)

	err = commitstatus.PostGitHubCompatible(context.Background(),
		scheme+"://"+host, host, "owner/repo", "deadbeef", "mytoken",
		commitstatus.Status{
			State:       commitstatus.StateSuccess,
			Description: "Deployed!",
			Context:     "ci/deploy",
		})
	assert.NilError(t, err)
	assert.Equal(t, received["state"], "success")
	assert.Equal(t, received["description"], "Deployed!")
	assert.Equal(t, received["context"], "ci/deploy")
}

func TestPost_GitHubEnterprise(t *testing.T) {
	t.Parallel()

	received := map[string]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Assert(t, strings.Contains(r.URL.Path, "/api/v3/repos/"), "unexpected path: %s", r.URL.Path)
		assert.Assert(t, strings.HasSuffix(r.URL.Path, "/statuses/deadbeef"), "unexpected path: %s", r.URL.Path)

		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	host, scheme, err := commitstatus.ParseHostAndScheme(srv.URL)
	assert.NilError(t, err)

	err = commitstatus.PostGitHub(context.Background(),
		scheme+"://"+host, host, "owner/repo", "deadbeef", "ghtoken",
		commitstatus.Status{
			State:       commitstatus.StateSuccess,
			Description: "GHE deploy",
			Context:     "ci/deploy",
		})
	assert.NilError(t, err)
	assert.Equal(t, received["state"], "success")
}

func TestPost_ExplicitProvider_GiteaOnGitLabHost(t *testing.T) {
	t.Parallel()

	received := map[string]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Assert(t, strings.Contains(r.URL.Path, "/api/v1/repos/"), "unexpected path: %s", r.URL.Path)

		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderGitea,
		srv.URL+"/owner/repo", "owner/repo", "abc123", "token",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.NilError(t, err)
}

func TestGet_GiteaAPI(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodGet)
		assert.Assert(t, strings.Contains(r.URL.Path, "/api/v1/repos/"), "unexpected path: %s", r.URL.Path)
		assert.Assert(t, strings.HasSuffix(r.URL.Path, "/statuses/deadbeef"), "unexpected path: %s", r.URL.Path)
		assert.Equal(t, r.Header.Get("Authorization"), "token "+"token")

		_ = json.NewEncoder(w).Encode([]map[string]string{
			{
				"state":       "success",
				"description": "Successful in 47s",
				"context":     commitstatus.DefaultContext,
				"target_url":  "https://example.com/logs/1",
			},
		})
	}))
	defer srv.Close()

	status, found, err := commitstatus.Get(context.Background(),
		commitstatus.ProviderGitea,
		srv.URL+"/owner/repo", "owner/repo", "deadbeef", "token", commitstatus.DefaultContext)
	assert.NilError(t, err)
	assert.Assert(t, found)
	assert.Equal(t, status.State, commitstatus.StateSuccess)
	assert.Equal(t, status.Description, "Successful in 47s")
	assert.Equal(t, status.TargetURL, "https://example.com/logs/1")
}
