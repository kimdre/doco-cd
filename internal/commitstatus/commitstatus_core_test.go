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
		{name: "https custom port preserved", url: "https://gitea.example.com:8443/owner/repo.git", wantHost: "gitea.example.com:8443", wantScheme: "https"},
		{name: "https gitlab", url: "https://gitlab.com/owner/repo.git", wantHost: "gitlab.com", wantScheme: "https"},
		{name: "https gitea", url: "https://gitea.example.com/owner/repo.git", wantHost: "gitea.example.com", wantScheme: "https"},
		{name: "ssh scp github", url: "git@github.com:owner/repo.git", wantHost: "github.com", wantScheme: "https"},
		{name: "ssh scp gitlab", url: "git@gitlab.com:owner/repo.git", wantHost: "gitlab.com", wantScheme: "https"},
		{name: "ssh url scheme", url: "ssh://git@github.com/owner/repo.git", wantHost: "github.com", wantScheme: "https"},
		{name: "ssh custom port stripped", url: "ssh://git@gitea.example.com:2222/owner/repo.git", wantHost: "gitea.example.com", wantScheme: "https"},
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
		"", "https://github.com/owner/repo", "owner/repo", "abc123", "",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.NilError(t, err)
}

func TestPost_NoopWhenMissingSHA(t *testing.T) {
	t.Parallel()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderAuto,
		"", "https://github.com/owner/repo", "owner/repo", "", "token",
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
		"", srv.URL+"/owner/repo", "owner/repo", "abc123", "token",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.NilError(t, err)
	assert.Equal(t, received["context"], commitstatus.BaseContext)
}

func TestContextForStack(t *testing.T) {
	t.Parallel()

	assert.Equal(t, commitstatus.ContextForStack("", ""), commitstatus.BaseContext)
	assert.Equal(t, commitstatus.ContextForStack("", "web"), "doco-cd/web")
	assert.Equal(t, commitstatus.ContextForStack("nas", "web"), "doco-cd/nas/web")
}

func TestPost_APIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderAuto,
		"", srv.URL+"/owner/repo", "owner/repo", "abc123", "badtoken",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.Assert(t, err != nil, "expected error for non-2xx response")

	if err != nil {
		assert.Assert(t, strings.Contains(err.Error(), "401"), "error should mention status code, got: %s", err.Error())
	}
}

func TestPost_APIError_AzureDevOpsLocalizedMessage(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"\u041d\u0435\u0434\u043e\u043f\u0443\u0441\u0442\u0438\u043c\u0430\u044f \u0432\u0435\u0442\u043a\u0430","typeName":"Microsoft.TeamFoundation.SourceControl.WebServer.InvalidRefNameException","errorCode":12345}`))
	}))
	defer srv.Close()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderAzureDevOps,
		"",
		srv.URL+"/org/project/_git/repo",
		"org/project/_git/repo",
		"abc123",
		"token",
		commitstatus.Status{State: commitstatus.StateFailure},
	)
	assert.Assert(t, err != nil, "expected error for non-2xx response")

	if err != nil {
		assert.Assert(t, strings.Contains(err.Error(), "Недопустимая ветка"), "error should contain decoded message, got: %s", err.Error())
		assert.Assert(t, strings.Contains(err.Error(), "typeName=Microsoft.TeamFoundation.SourceControl.WebServer.InvalidRefNameException"), "error should contain typeName, got: %s", err.Error())
		assert.Assert(t, strings.Contains(err.Error(), "errorCode=12345"), "error should contain errorCode, got: %s", err.Error())
		assert.Assert(t, !strings.Contains(err.Error(), "\\u041d"), "error should not contain escaped unicode, got: %s", err.Error())
	}
}

func TestPost_APIError_FallsBackToRawBody(t *testing.T) {
	t.Parallel()

	const rawBody = `not-json body content`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(rawBody))
	}))
	defer srv.Close()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderAuto,
		"",
		srv.URL+"/owner/repo",
		"owner/repo",
		"abc123",
		"token",
		commitstatus.Status{State: commitstatus.StateFailure},
	)
	assert.Assert(t, err != nil, "expected error for non-2xx response")

	if err != nil {
		assert.Assert(t, strings.Contains(err.Error(), rawBody), "error should contain raw body when JSON parsing fails, got: %s", err.Error())
	}
}

func TestPost_APIError_DecodesJSONStringBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`"\u041d\u0435\u0434\u043e\u0441\u0442\u0443\u043f\u043d\u043e"`))
	}))
	defer srv.Close()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderAuto,
		"",
		srv.URL+"/owner/repo",
		"owner/repo",
		"abc123",
		"token",
		commitstatus.Status{State: commitstatus.StateFailure},
	)
	assert.Assert(t, err != nil, "expected error for non-2xx response")

	if err != nil {
		assert.Assert(t, strings.Contains(err.Error(), "Недоступно"), "error should contain decoded JSON string message, got: %s", err.Error())
		assert.Assert(t, !strings.Contains(err.Error(), "\\u041d"), "error should not contain escaped unicode, got: %s", err.Error())
	}
}

func TestPost_UsesConfiguredAPIBaseURL(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/api/v1/repos/owner/repo/statuses/abc123")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderGitea,
		srv.URL,
		"ssh://git@gitea.example.com:2222/owner/repo.git",
		"owner/repo", "abc123", "token",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.NilError(t, err)
}

func TestPost_RetriesTransientAPIErrors(t *testing.T) {
	t.Parallel()

	attempts := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderGitea,
		"", srv.URL+"/owner/repo", "owner/repo", "abc123", "token",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.NilError(t, err)
	assert.Equal(t, attempts, 3)
}

func TestPost_DoesNotRetryPermanentAPIErrors(t *testing.T) {
	t.Parallel()

	attempts := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++

		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderGitea,
		"", srv.URL+"/owner/repo", "owner/repo", "abc123", "token",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.Assert(t, err != nil, "expected post to fail for 401")
	assert.Equal(t, attempts, 1)
}

func TestPost_InvalidConfiguredAPIBaseURL(t *testing.T) {
	t.Parallel()

	err := commitstatus.Post(context.Background(),
		commitstatus.ProviderGitea,
		"ssh://gitea.example.com:2222",
		"https://gitea.example.com/owner/repo.git",
		"owner/repo", "abc123", "token",
		commitstatus.Status{State: commitstatus.StateSuccess})
	assert.Assert(t, err != nil, "expected invalid scm api url to fail")

	if err == nil {
		t.Fatal("expected error for invalid scm api url")
	}

	assert.Assert(t, strings.Contains(err.Error(), "failed to parse scm API base URL"))
}
