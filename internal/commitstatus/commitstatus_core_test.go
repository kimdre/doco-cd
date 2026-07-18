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

	assert.Equal(t, commitstatus.ResolveProvider(commitstatus.ProviderGitLab, "github.com"), commitstatus.ProviderGitLab)
	assert.Equal(t, commitstatus.ResolveProvider(commitstatus.ProviderGitHub, "git.mycompany.com"), commitstatus.ProviderGitHub)
	assert.Equal(t, commitstatus.ResolveProvider(commitstatus.ProviderGitea, "gitlab.com"), commitstatus.ProviderGitea)
	assert.Equal(t, commitstatus.ResolveProvider(commitstatus.ProviderAzureDevOps, "github.com"), commitstatus.ProviderAzureDevOps)
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

func TestPost_DefaultContext(t *testing.T) {
	t.Parallel()

	received := map[string]string{}

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
