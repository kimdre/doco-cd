package git

import (
	"errors"
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

func TestIsLocalFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url  string
		want bool
	}{
		{"file:///data/local-repos/my-app", true},
		{"https://example.com/repo.git", false},
		{"ssh://git@example.com/repo.git", false},
		{"git@example.com:owner/repo.git", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := IsLocalFile(tt.url); got != tt.want {
			t.Errorf("IsLocalFile(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func TestGetAuthMethod_LocalFileNeedsNoCredentials(t *testing.T) {
	ConfigureAuthResolver(nil, "", "", "global-token", "", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{})
	})

	auth, err := GetAuthMethod("file:///data/local-repos/my-app", "", "", "")
	if err != nil {
		t.Fatalf("GetAuthMethod() error = %v, want nil", err)
	}

	if auth != nil {
		t.Fatalf("GetAuthMethod() = %v, want nil auth for local file URL", auth)
	}
}

func TestResolveScopedCredentials_ExactBeatsWildcard(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{
		{
			Domains:        []string{"*.github.com"},
			GitAccessToken: "wildcard-token",
		},
		{
			Domains:        []string{"api.github.com"},
			GitAccessToken: "exact-token",
		},
	}, "", "", "", "", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{})
	})

	_, _, token := ResolveScopedCredentials("https://api.github.com/org/repo.git", "", "", "")
	if token != "exact-token" {
		t.Fatalf("expected exact token to win, got '%s'", token)
	}
}

func TestResolveScopedCredentials_LongestWildcardSuffixWins(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{
		{
			Domains:        []string{"*.example.com"},
			GitAccessToken: "broad-token",
		},
		{
			Domains:        []string{"*.foo.example.com"},
			GitAccessToken: "specific-token",
		},
	}, "", "", "", "", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{})
	})

	_, _, token := ResolveScopedCredentials("https://git.foo.example.com/org/repo.git", "", "", "")
	if token != "specific-token" {
		t.Fatalf("expected most specific wildcard token, got '%s'", token)
	}
}

func TestResolveScopedCredentials_WildcardDoesNotMatchApex(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{
		{
			Domains:        []string{"*.example.com"},
			GitAccessToken: "wildcard-token",
		},
	}, "", "", "", "", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{})
	})

	_, _, token := ResolveScopedCredentials("https://example.com/org/repo.git", "", "", "")
	if token != "" {
		t.Fatalf("expected no wildcard match for apex domain, got '%s'", token)
	}
}

func TestResolveScopedCredentials_GlobalFallback(t *testing.T) {
	ConfigureAuthResolver(nil, "", "", "global-token", "", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{})
	})

	_, _, token := ResolveScopedCredentials("https://gitlab.com/group/repo.git", "", "", "")
	if token != "global-token" {
		t.Fatalf("expected global fallback token, got '%s'", token)
	}
}

func TestGetAuthMethod_UsesScopedHTTPToken(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{
		{
			Domains:        []string{"gitlab.com"},
			GitAccessToken: "scoped-token",
		},
	}, "", "", "", "", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{})
	})

	auth, err := GetAuthMethod("https://gitlab.com/group/repo.git", "", "", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if auth == nil {
		t.Fatal("expected auth method, got nil")
	}

	if auth.Name() != "http-basic-auth" {
		t.Fatalf("expected http-basic-auth, got '%s'", auth.Name())
	}
}

func TestGetAuthMethod_ScopedTokenUserSetsHTTPUsername(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{
		{
			Domains:            []string{"gitlab.com"},
			GitAccessTokenUser: "gitlab+deploy-token-123",
			GitAccessToken:     "scoped-token",
		},
	}, "", "", "", "oauth2", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{})
	})

	auth, err := GetAuthMethod("https://gitlab.com/group/repo.git", "", "", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	basicAuth, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("expected *githttp.BasicAuth, got %T", auth)
	}

	if basicAuth.Username != "gitlab+deploy-token-123" {
		t.Fatalf("expected scoped git_access_token_user as username, got '%s'", basicAuth.Username)
	}
}

func TestGetAuthMethod_GlobalHTTPAuthUserFallback(t *testing.T) {
	ConfigureAuthResolver(nil, "", "", "global-token", "global-user", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{})
	})

	auth, err := GetAuthMethod("https://gitlab.com/group/repo.git", "", "", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	basicAuth, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("expected *githttp.BasicAuth, got %T", auth)
	}

	if basicAuth.Username != "global-user" {
		t.Fatalf("expected global auth type as username, got '%s'", basicAuth.Username)
	}
}

func TestGetAuthMethod_UsesGlobalGitHubAppToken(t *testing.T) {
	ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{
		ID:         "12345",
		PrivateKey: "test-private-key",
	})

	oldProvider := SwapGitHubAppTokenProviderForTest(func(_ string, _ GitHubAppConfig) (string, error) {
		return "ghs-install-token", nil
	})

	t.Cleanup(func() {
		oldProvider()
		ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{})
	})

	auth, err := GetAuthMethod("https://github.com/org/repo.git", "", "", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if auth == nil {
		t.Fatal("expected auth method, got nil")
	}

	if auth.Name() != "http-basic-auth" {
		t.Fatalf("expected http-basic-auth, got '%s'", auth.Name())
	}
}

func TestResolveHTTPToken_PrefersExplicitToken(t *testing.T) {
	resolved := ResolvedAuthConfig{
		GitAccessToken: "explicit-token",
		GitHubApp:      GitHubAppConfig{ID: "12345", PrivateKey: "test-private-key"},
	}

	oldProvider := SwapGitHubAppTokenProviderForTest(func(_ string, _ GitHubAppConfig) (string, error) {
		t.Fatal("github app token provider should not be called when an explicit token is set")
		return "", nil
	})
	t.Cleanup(oldProvider)

	token, err := ResolveHTTPToken("https://github.com/org/repo.git", resolved)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if token != "explicit-token" {
		t.Fatalf("expected explicit token to win, got '%s'", token)
	}
}

func TestResolveHTTPToken_FallsBackToGitHubApp(t *testing.T) {
	resolved := ResolvedAuthConfig{
		GitHubApp: GitHubAppConfig{ID: "12345", PrivateKey: "test-private-key"},
	}

	oldProvider := SwapGitHubAppTokenProviderForTest(func(_ string, cfg GitHubAppConfig) (string, error) {
		if cfg.ID != "12345" {
			t.Fatalf("expected app id 12345, got %s", cfg.ID)
		}

		return "ghs-install-token", nil // #nosec G101 -- test fixture, not a real credential
	})
	t.Cleanup(oldProvider)

	token, err := ResolveHTTPToken("https://github.com/org/repo.git", resolved)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if token != "ghs-install-token" { // #nosec G101 -- test fixture, not a real credential
		t.Fatalf("expected github app installation token, got '%s'", token)
	}
}

func TestResolveHTTPToken_ReturnsErrorFromGitHubAppProvider(t *testing.T) {
	resolved := ResolvedAuthConfig{
		GitHubApp: GitHubAppConfig{ID: "12345", PrivateKey: "test-private-key"},
	}

	oldProvider := SwapGitHubAppTokenProviderForTest(func(_ string, _ GitHubAppConfig) (string, error) {
		return "", errors.New("boom")
	})
	t.Cleanup(oldProvider)

	_, err := ResolveHTTPToken("https://github.com/org/repo.git", resolved)
	if err == nil {
		t.Fatal("expected an error when the github app token provider fails")
	}
}

func TestResolveHTTPToken_EmptyWhenNoCredentialsConfigured(t *testing.T) {
	token, err := ResolveHTTPToken("https://github.com/org/repo.git", ResolvedAuthConfig{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if token != "" {
		t.Fatalf("expected empty token, got '%s'", token)
	}
}

func TestGetAuthMethod_UsesScopedGitHubAppToken(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{
		{
			Domains:             []string{"github.com"},
			GitHubAppID:         "99999",
			GitHubAppPrivateKey: "scoped-private-key",
		},
	}, "", "", "", "", GitHubAppConfig{})

	oldProvider := SwapGitHubAppTokenProviderForTest(func(_ string, cfg GitHubAppConfig) (string, error) {
		if cfg.ID != "99999" {
			t.Fatalf("expected scoped app id 99999, got %s", cfg.ID)
		}

		return "ghs-scoped-install-token", nil
	})

	t.Cleanup(func() {
		oldProvider()
		ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{})
	})

	auth, err := GetAuthMethod("https://github.com/org/repo.git", "", "", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if auth == nil {
		t.Fatal("expected auth method, got nil")
	}

	if auth.Name() != "http-basic-auth" {
		t.Fatalf("expected http-basic-auth, got '%s'", auth.Name())
	}
}
