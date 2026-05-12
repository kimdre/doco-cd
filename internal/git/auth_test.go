package git

import (
	"testing"
)

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
	}, "", "", "", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", GitHubAppConfig{})
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
	}, "", "", "", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", GitHubAppConfig{})
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
	}, "", "", "", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", GitHubAppConfig{})
	})

	_, _, token := ResolveScopedCredentials("https://example.com/org/repo.git", "", "", "")
	if token != "" {
		t.Fatalf("expected no wildcard match for apex domain, got '%s'", token)
	}
}

func TestResolveScopedCredentials_GlobalFallback(t *testing.T) {

	ConfigureAuthResolver(nil, "", "", "global-token", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", GitHubAppConfig{})
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
	}, "", "", "", GitHubAppConfig{})
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", GitHubAppConfig{})
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

func TestGetAuthMethod_UsesGlobalGitHubAppToken(t *testing.T) {

	ConfigureAuthResolver(nil, "", "", "", GitHubAppConfig{
		ID:         "12345",
		PrivateKey: "test-private-key",
	})

	oldProvider := swapGitHubAppTokenProviderForTest(func(_ string, _ GitHubAppConfig) (string, error) {
		return "ghs-install-token", nil
	})

	t.Cleanup(func() {
		oldProvider()
		ConfigureAuthResolver(nil, "", "", "", GitHubAppConfig{})
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

func TestGetAuthMethod_UsesScopedGitHubAppToken(t *testing.T) {

	ConfigureAuthResolver([]ScopedAuthConfig{
		{
			Domains:             []string{"github.com"},
			GitHubAppID:         "99999",
			GitHubAppPrivateKey: "scoped-private-key",
		},
	}, "", "", "", GitHubAppConfig{})

	oldProvider := swapGitHubAppTokenProviderForTest(func(_ string, cfg GitHubAppConfig) (string, error) {
		if cfg.ID != "99999" {
			t.Fatalf("expected scoped app id 99999, got %s", cfg.ID)
		}

		return "ghs-scoped-install-token", nil
	})

	t.Cleanup(func() {
		oldProvider()
		ConfigureAuthResolver(nil, "", "", "", GitHubAppConfig{})
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

