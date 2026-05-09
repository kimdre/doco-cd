package git_test

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/git"
)

func TestResolveScopedCredentials_ExactBeatsWildcard(t *testing.T) {
	t.Parallel()

	git.ConfigureAuthResolver([]git.ScopedAuthConfig{
		{
			Domains:        []string{"*.github.com"},
			GitAccessToken: "wildcard-token",
		},
		{
			Domains:        []string{"api.github.com"},
			GitAccessToken: "exact-token",
		},
	}, "", "", "")
	t.Cleanup(func() {
		git.ConfigureAuthResolver(nil, "", "", "")
	})

	_, _, token := git.ResolveScopedCredentials("https://api.github.com/org/repo.git", "", "", "")
	if token != "exact-token" {
		t.Fatalf("expected exact token to win, got '%s'", token)
	}
}

func TestResolveScopedCredentials_LongestWildcardSuffixWins(t *testing.T) {
	t.Parallel()

	git.ConfigureAuthResolver([]git.ScopedAuthConfig{
		{
			Domains:        []string{"*.example.com"},
			GitAccessToken: "broad-token",
		},
		{
			Domains:        []string{"*.foo.example.com"},
			GitAccessToken: "specific-token",
		},
	}, "", "", "")
	t.Cleanup(func() {
		git.ConfigureAuthResolver(nil, "", "", "")
	})

	_, _, token := git.ResolveScopedCredentials("https://git.foo.example.com/org/repo.git", "", "", "")
	if token != "specific-token" {
		t.Fatalf("expected most specific wildcard token, got '%s'", token)
	}
}

func TestResolveScopedCredentials_WildcardDoesNotMatchApex(t *testing.T) {
	t.Parallel()

	git.ConfigureAuthResolver([]git.ScopedAuthConfig{
		{
			Domains:        []string{"*.example.com"},
			GitAccessToken: "wildcard-token",
		},
	}, "", "", "")
	t.Cleanup(func() {
		git.ConfigureAuthResolver(nil, "", "", "")
	})

	_, _, token := git.ResolveScopedCredentials("https://example.com/org/repo.git", "", "", "")
	if token != "" {
		t.Fatalf("expected no wildcard match for apex domain, got '%s'", token)
	}
}

func TestResolveScopedCredentials_GlobalFallback(t *testing.T) {
	t.Parallel()

	git.ConfigureAuthResolver(nil, "", "", "global-token")
	t.Cleanup(func() {
		git.ConfigureAuthResolver(nil, "", "", "")
	})

	_, _, token := git.ResolveScopedCredentials("https://gitlab.com/group/repo.git", "", "", "")
	if token != "global-token" {
		t.Fatalf("expected global fallback token, got '%s'", token)
	}
}

func TestGetAuthMethod_UsesScopedHTTPToken(t *testing.T) {
	t.Parallel()

	git.ConfigureAuthResolver([]git.ScopedAuthConfig{
		{
			Domains:        []string{"gitlab.com"},
			GitAccessToken: "scoped-token",
		},
	}, "", "", "")
	t.Cleanup(func() {
		git.ConfigureAuthResolver(nil, "", "", "")
	})

	auth, err := git.GetAuthMethod("https://gitlab.com/group/repo.git", "", "", "")
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
