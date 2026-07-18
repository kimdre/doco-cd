package commitstatus_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/kimdre/doco-cd/internal/commitstatus"
)

func TestParseProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input   string
		want    commitstatus.Provider
		wantErr bool
	}{
		{input: "", want: commitstatus.ProviderAuto},
		{input: "auto", want: commitstatus.ProviderAuto},
		{input: "AUTO", want: commitstatus.ProviderAuto},
		{input: "github", want: commitstatus.ProviderGitHub},
		{input: "GITHUB", want: commitstatus.ProviderGitHub},
		{input: "GitHub", want: commitstatus.ProviderGitHub},
		{input: "gitlab", want: commitstatus.ProviderGitLab},
		{input: "gitea", want: commitstatus.ProviderGitea},
		{input: "forgejo", want: commitstatus.ProviderGitea}, // alias for gitea
		{input: "FORGEJO", want: commitstatus.ProviderGitea},
		{input: "azuredevops", want: commitstatus.ProviderAzureDevOps},
		{input: "unknown", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()

			got, err := commitstatus.ParseProvider(tc.input)
			if tc.wantErr {
				assert.Assert(t, err != nil, "expected error for %q", tc.input)

				return
			}

			assert.NilError(t, err)
			assert.Equal(t, got, tc.want)
		})
	}
}

func TestResolveProvider_AutoDetect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host string
		want commitstatus.Provider
	}{
		{"github.com", commitstatus.ProviderGitHub},
		{"github.com:443", commitstatus.ProviderGitHub},
		{"gitlab.com", commitstatus.ProviderGitLab},
		{"gitlab.mycompany.com", commitstatus.ProviderGitea}, // subdomain → unknown → gitea
		{"git.mycompany.com", commitstatus.ProviderGitea},    // unknown → gitea
		{"gitea.example.com", commitstatus.ProviderGitea},
		{"dev.azure.com", commitstatus.ProviderAzureDevOps},
		{"dev.azure.com:443", commitstatus.ProviderAzureDevOps},
		{"ssh.dev.azure.com", commitstatus.ProviderAzureDevOps},
		{"my-org.visualstudio.com", commitstatus.ProviderAzureDevOps},
	}

	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, commitstatus.ResolveProvider(commitstatus.ProviderAuto, tc.host), tc.want)
		})
	}
}

func TestResolveProvider_ExplicitOverride(t *testing.T) {
	t.Parallel()

	// An explicit provider must win over URL heuristics.
	assert.Equal(
		t,
		commitstatus.ResolveProvider(commitstatus.ProviderGitLab, "github.com"),
		commitstatus.ProviderGitLab,
	)
	assert.Equal(
		t,
		commitstatus.ResolveProvider(commitstatus.ProviderGitHub, "git.mycompany.com"),
		commitstatus.ProviderGitHub,
	)
	assert.Equal(
		t,
		commitstatus.ResolveProvider(commitstatus.ProviderGitea, "gitlab.com"),
		commitstatus.ProviderGitea,
	)
	assert.Equal(
		t,
		commitstatus.ResolveProvider(commitstatus.ProviderAzureDevOps, "github.com"),
		commitstatus.ProviderAzureDevOps,
	)
}

func TestParseHostAndScheme(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		url        string
		wantHost   string
		wantScheme string
		wantErr    bool
	}{
		{name: "https github", url: "https://github.com/owner/repo.git", wantHost: "github.com", wantScheme: "https"},
		{name: "https gitlab", url: "https://gitlab.com/owner/repo.git", wantHost: "gitlab.com", wantScheme: "https"},
		{name: "https gitea", url: "https://gitea.example.com/owner/repo.git", wantHost: "gitea.example.com", wantScheme: "https"},
		{name: "ssh scp github", url: "git@github.com:owner/repo.git", wantHost: "github.com", wantScheme: "https"},
		{name: "ssh scp gitlab", url: "git@gitlab.com:owner/repo.git", wantHost: "gitlab.com", wantScheme: "https"},
		{name: "ssh url scheme", url: "ssh://git@github.com/owner/repo.git", wantHost: "github.com", wantScheme: "https"},
		{name: "empty url", url: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			host, scheme, err := commitstatus.ParseHostAndScheme(tc.url)
			if tc.wantErr {
				assert.Assert(t, err != nil, "expected error for URL %q", tc.url)

				return
			}

			assert.NilError(t, err)
			assert.Equal(t, host, tc.wantHost)
			assert.Equal(t, scheme, tc.wantScheme)
		})
	}
}

func TestPost_NoopWhenMissingToken(t *testing.T) {
	t.Parallel()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderAuto,
		"https://github.com/owner/repo", "owner/repo", "abc123", "",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.NilError(t, err)
}

func TestPost_NoopWhenMissingSHA(t *testing.T) {
	t.Parallel()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderAuto,
		"https://github.com/owner/repo", "owner/repo", "", "token",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.NilError(t, err)
}

func TestPost_GiteaAPI(t *testing.T) {
	t.Parallel()

	var received map[string]string

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

	var received map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GHE endpoint: /api/v3/repos/{owner}/{repo}/statuses/{sha}
		assert.Assert(t, strings.Contains(r.URL.Path, "/api/v3/repos/"), "unexpected path: %s", r.URL.Path)
		assert.Assert(t, strings.HasSuffix(r.URL.Path, "/statuses/deadbeef"), "unexpected path: %s", r.URL.Path)

		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))

	defer srv.Close()

	host, scheme, err := commitstatus.ParseHostAndScheme(srv.URL)
	assert.NilError(t, err)

	// Use explicit ProviderGitHub on a non-github.com host to exercise GHE path.
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

func TestPost_ExplicitProvider_GitLabOnUnexpectedHost(t *testing.T) {
	t.Parallel()

	var received map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GitLab v4 endpoint: /api/v4/projects/{encoded}/statuses/{sha}
		assert.Assert(t, strings.Contains(r.URL.Path, "/api/v4/projects/"), "unexpected path: %s", r.URL.Path)

		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))

	defer srv.Close()

	// Force GitLab even though the URL has no "gitlab" in it.
	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderGitLab,
		srv.URL+"/owner/repo", "owner/repo", "abc123", "token",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.NilError(t, err)
	assert.Equal(t, received["state"], "success")
}

func TestPost_ExplicitProvider_GiteaOnGitLabHost(t *testing.T) {
	t.Parallel()

	var received map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Gitea v1 endpoint expected even though URL path starts with gitlab.
		assert.Assert(t, strings.Contains(r.URL.Path, "/api/v1/repos/"), "unexpected path: %s", r.URL.Path)

		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))

	defer srv.Close()

	// Force Gitea even though the URL host is gitlab.com (which would normally auto-detect as GitLab).
	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderGitea,
		srv.URL+"/owner/repo", "owner/repo", "abc123", "token",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.NilError(t, err)
}

func TestPost_GitLabAPI_Failure(t *testing.T) {
	t.Parallel()

	var received map[string]string

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

	var received map[string]string

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

	var received map[string]string

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

func TestPost_AzureDevOpsAPI(t *testing.T) {
	t.Parallel()

	var received map[string]any
	expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(":token"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/org/project/_apis/git/repositories/repo/commits/deadbeef/statuses")
		assert.Equal(t, r.URL.Query().Get("api-version"), "7.1")
		assert.Equal(t, r.Header.Get("Authorization"), expectedAuth)

		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))

	defer srv.Close()

	err := commitstatus.Post(
		context.Background(),
		commitstatus.ProviderAzureDevOps,
		srv.URL+"/org/project/_git/repo",
		"org/project/_git/repo",
		"deadbeef",
		"token",
		commitstatus.Status{
			State:       commitstatus.StateSuccess,
			Description: "Successful in 47s",
			Context:     "doco-cd/deploy",
			TargetURL:   "https://example.com/logs/1",
		},
	)
	assert.NilError(t, err)
	assert.Equal(t, received["state"], "succeeded")
	assert.Equal(t, received["description"], "Successful in 47s")
	contextData, ok := received["context"].(map[string]any)
	assert.Assert(t, ok)
	assert.Equal(t, contextData["name"], "doco-cd/deploy")
	assert.Equal(t, contextData["genre"], "doco-cd")
	assert.Equal(t, received["targetUrl"], "https://example.com/logs/1")
}

func TestPost_DefaultContext(t *testing.T) {
	t.Parallel()

	var received map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)

		w.WriteHeader(http.StatusCreated)
	}))

	defer srv.Close()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderAuto,
		srv.URL+"/owner/repo", "owner/repo", "abc123", "token",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.NilError(t, err)
	assert.Equal(t, received["context"], commitstatus.DefaultContext)
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

func TestGet_GitLabAPI(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodGet)
		assert.Assert(t, strings.Contains(r.URL.Path, "/api/v4/projects/"), "unexpected path: %s", r.URL.Path)
		assert.Assert(t, strings.HasSuffix(r.URL.Path, "/repository/commits/deadbeef/statuses"), "unexpected path: %s", r.URL.Path)

		_ = json.NewEncoder(w).Encode([]map[string]string{
			{
				"status":      "success",
				"name":        commitstatus.DefaultContext,
				"description": "Successful in 47s",
				"target_url":  "https://example.com/logs/1",
			},
		})
	}))
	defer srv.Close()

	status, found, err := commitstatus.Get(context.Background(),
		commitstatus.ProviderGitLab,
		srv.URL+"/owner/repo", "owner/repo", "deadbeef", "token", commitstatus.DefaultContext)
	assert.NilError(t, err)
	assert.Assert(t, found)
	assert.Equal(t, status.State, commitstatus.StateSuccess)
	assert.Equal(t, status.Description, "Successful in 47s")
}

func TestGet_AzureDevOpsAPI(t *testing.T) {
	t.Parallel()

	expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(":token"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodGet)
		assert.Equal(t, r.URL.Path, "/org/project/_apis/git/repositories/repo/commits/deadbeef/statuses")
		assert.Equal(t, r.URL.Query().Get("api-version"), "7.1")
		assert.Equal(t, r.Header.Get("Authorization"), expectedAuth)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]any{
				{
					"state":       "succeeded",
					"description": "Successful in 47s",
					"targetUrl":   "https://example.com/logs/1",
					"context": map[string]string{
						"name":  commitstatus.DefaultContext,
						"genre": "doco-cd",
					},
				},
			},
		})
	}))
	defer srv.Close()

	status, found, err := commitstatus.Get(context.Background(),
		commitstatus.ProviderAzureDevOps,
		srv.URL+"/org/project/_git/repo",
		"org/project/_git/repo",
		"deadbeef",
		"token",
		commitstatus.DefaultContext)
	assert.NilError(t, err)
	assert.Assert(t, found)
	assert.Equal(t, status.State, commitstatus.StateSuccess)
	assert.Equal(t, status.Description, "Successful in 47s")
	assert.Equal(t, status.TargetURL, "https://example.com/logs/1")
}

func TestPost_APIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))

	defer srv.Close()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderAuto,
		srv.URL+"/owner/repo", "owner/repo", "abc123", "badtoken",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.Assert(t, err != nil, "expected error for non-2xx response")
	assert.Assert(t, strings.Contains(err.Error(), "401"), "error should mention status code, got: %s", err.Error())
}
