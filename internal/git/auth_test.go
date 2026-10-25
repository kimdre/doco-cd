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

func TestIsNativeGit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url  string
		want bool
	}{
		{"git://example.com/repo.git", true},
		{"GIT://example.com/repo.git", true},
		{"git://", true},
		{"git:/example.com/repo.git", false},
		{"git@example.com:owner/repo.git", false},
		{"https://example.com/repo.git", false},
		{"ssh://git@example.com/repo.git", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := IsNativeGit(tt.url); got != tt.want {
			t.Errorf("IsNativeGit(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func resetAuthResolver(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{})
	})
}

func TestGetAuthMethod_LocalFileNeedsNoCredentials(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{{Domains: []string{"example.com"}}}, "", "", "global-token", "", GitHubAppConfig{})
	resetAuthResolver(t)

	auth, err := GetAuthMethod("file:///data/local-repos/my-app", "", "")
	if err != nil {
		t.Fatalf("GetAuthMethod() error = %v, want nil", err)
	}

	if auth != nil {
		t.Fatalf("GetAuthMethod() = %v, want nil auth for local file URL", auth)
	}
}

func TestGetAuthMethod_NativeGitNeedsNoCredentials(t *testing.T) {
	providerCalls := 0
	restoreProvider := SwapGitHubAppTokenProviderForTest(func(string, GitHubAppConfig) (string, error) {
		providerCalls++

		return "", errors.New("native Git must not resolve a GitHub App token")
	})
	t.Cleanup(restoreProvider)
	resetAuthResolver(t)

	domainsOnly := []ScopedAuthConfig{{Domains: []string{"example.com"}}}

	tests := []struct {
		name       string
		scoped     []ScopedAuthConfig
		global     ResolvedAuthConfig
		privateKey string
	}{
		{name: "no credentials"},
		{name: "explicit credentials", privateKey: "explicit-key"},
		{
			name:   "global credentials",
			scoped: domainsOnly,
			global: ResolvedAuthConfig{SSHPrivateKey: "global-key", GitAccessToken: "global-token"},
		},
		{
			name: "scoped credentials",
			scoped: []ScopedAuthConfig{{
				Domains:        []string{"example.com"},
				GitAccessToken: "scoped-token",
				SSHPrivateKey:  "scoped-key",
			}},
		},
		{
			name:   "global GitHub App",
			scoped: domainsOnly,
			global: ResolvedAuthConfig{GitHubApp: GitHubAppConfig{
				ID: "global-app", PrivateKey: "global-app-key",
			}},
		},
		{
			name: "scoped GitHub App",
			scoped: []ScopedAuthConfig{{
				Domains:             []string{"example.com"},
				GitHubAppID:         "scoped-app",
				GitHubAppPrivateKey: "scoped-app-key",
			}},
		},
		{name: "SSH key", global: ResolvedAuthConfig{SSHPrivateKey: "global-key"}},
	}

	for _, tt := range tests {
		for _, url := range []string{"git://example.com:19418/repo.git", "GIT://example.com/repo.git"} {
			t.Run(tt.name+"/"+url, func(t *testing.T) {
				ConfigureAuthResolver(tt.scoped, tt.global.SSHPrivateKey, "", tt.global.GitAccessToken, "", tt.global.GitHubApp)

				auth, err := GetAuthMethod(url, tt.privateKey, "")
				if err != nil || auth != nil {
					t.Fatalf("GetAuthMethod() = %v, %v, want nil auth and error", auth, err)
				}

				if providerCalls != 0 {
					t.Fatalf("GitHub App token provider called %d times", providerCalls)
				}
			})
		}
	}
}

func TestResolveAuthConfig_ExactBeatsWildcard(t *testing.T) {
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
	resetAuthResolver(t)

	token := ResolveAuthConfig("https://api.github.com/org/repo.git", "", "").GitAccessToken
	if token != "exact-token" {
		t.Fatalf("expected exact token to win, got '%s'", token)
	}
}

func TestResolveAuthConfig_LongestWildcardSuffixWins(t *testing.T) {
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
	resetAuthResolver(t)

	token := ResolveAuthConfig("https://git.foo.example.com/org/repo.git", "", "").GitAccessToken
	if token != "specific-token" {
		t.Fatalf("expected most specific wildcard token, got '%s'", token)
	}
}

func TestResolveAuthConfig_WildcardDoesNotMatchApex(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{
		{
			Domains:        []string{"*.example.com"},
			GitAccessToken: "wildcard-token",
		},
	}, "", "", "", "", GitHubAppConfig{})
	resetAuthResolver(t)

	token := ResolveAuthConfig("https://example.com/org/repo.git", "", "").GitAccessToken
	if token != "" {
		t.Fatalf("expected no wildcard match for apex domain, got '%s'", token)
	}
}

func TestResolveAuthConfig_NormalizesHost(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{{Domains: []string{"GitHub.com."}}}, "", "", "global-token", "", GitHubAppConfig{})
	resetAuthResolver(t)

	url := "https://GITHUB.com./org/repo.git"

	resolved := ResolveAuthConfig(url, "", "")
	if resolved.GitAccessToken != "global-token" {
		t.Fatalf("expected global token for a normalized host match, got '%s'", resolved.GitAccessToken)
	}

	token, err := ResolveHTTPToken("https://github.com/org/repo.git", resolved)
	if err != nil || token != "global-token" {
		t.Fatalf("ResolveHTTPToken() = %q, %v, want global token", token, err)
	}
}

// The global token must never reach a host that GIT_AUTH_DOMAINS does not list.
func TestResolveAuthConfig_GlobalTokenNotUsedForUnscopedHost(t *testing.T) {
	tests := []struct {
		name   string
		scoped []ScopedAuthConfig
	}{
		{name: "no scoped entries"},
		{name: "other host scoped", scoped: []ScopedAuthConfig{{Domains: []string{"github.com"}}}},
		{name: "wildcard does not match", scoped: []ScopedAuthConfig{{Domains: []string{"*.github.com"}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ConfigureAuthResolver(tt.scoped, "", "", "global-token", "global-user", GitHubAppConfig{})
			resetAuthResolver(t)

			for _, url := range []string{
				"https://attacker.example/org/repo.git",
				"http://github.com.attacker.example/org/repo.git",
				"https://127.0.0.1:8443/org/repo.git",
			} {
				resolved := ResolveAuthConfig(url, "", "")
				if resolved.HTTPMethod() != HTTPAuthNone || resolved.HTTPAuthUser != "" {
					t.Fatalf("ResolveAuthConfig(%q) selected an HTTP credential: %+v", url, resolved)
				}

				auth, err := GetAuthMethod(url, "", "")
				if err != nil || auth != nil {
					t.Fatalf("GetAuthMethod(%q) = %v, %v, want anonymous access", url, auth, err)
				}
			}
		})
	}
}

// WithAllowAnyHost is the explicit opt-out: unmatched hosts get the global credential,
// but a matched entry still uses its own credential.
func TestResolveAuthConfig_AllowAnyHost(t *testing.T) {
	scoped := []ScopedAuthConfig{{Domains: []string{"gitlab.com"}, GitAccessToken: "gitlab-token"}}

	ConfigureAuthResolver(scoped, "", "", "global-token", "global-user", GitHubAppConfig{}, WithAllowAnyHost(true))
	resetAuthResolver(t)

	const unmatched = "https://other.example/org/repo.git"

	resolved := ResolveAuthConfig(unmatched, "", "")
	if resolved.GitAccessToken != "global-token" || resolved.HTTPAuthUser != "global-user" {
		t.Fatalf("ResolveAuthConfig(%q) = %+v, want the global token", unmatched, resolved)
	}

	token, err := ResolveHTTPToken(unmatched, resolved)
	if err != nil || token != "global-token" {
		t.Fatalf("ResolveHTTPToken(%q) = %q, %v, want the global token", unmatched, token, err)
	}

	// The credential stays bound to the host it was resolved for.
	if _, err = ResolveHTTPToken("https://elsewhere.example/org/repo.git", resolved); !errors.Is(err, ErrCredentialHostMismatch) {
		t.Fatalf("ResolveHTTPToken() for another host error = %v, want %v", err, ErrCredentialHostMismatch)
	}

	if resolved = ResolveAuthConfig("https://gitlab.com/org/repo.git", "", ""); resolved.GitAccessToken != "gitlab-token" {
		t.Fatalf("matched entry token = %q, want gitlab-token", resolved.GitAccessToken)
	}

	// Without the option, the same configuration sends nothing to unmatched hosts.
	ConfigureAuthResolver(scoped, "", "", "global-token", "global-user", GitHubAppConfig{}, WithAllowAnyHost(false))

	if resolved = ResolveAuthConfig(unmatched, "", ""); resolved.HTTPMethod() != HTTPAuthNone {
		t.Fatalf("ResolveAuthConfig(%q) without opt-out = %+v, want no HTTP credential", unmatched, resolved)
	}
}

// A global GitHub App is not used for gists, also when unmatched hosts are allowed.
func TestResolveAuthConfig_AllowAnyHostSkipsGitHubAppForGist(t *testing.T) {
	app := GitHubAppConfig{ID: "1", PrivateKey: "key"}

	ConfigureAuthResolver(nil, "", "", "", "", app, WithAllowAnyHost(true))
	resetAuthResolver(t)

	if resolved := ResolveAuthConfig("https://github.com/org/repo.git", "", ""); resolved.HTTPMethod() != HTTPAuthGitHubApp {
		t.Fatalf("repository resolved %+v, want the global GitHub App", resolved)
	}

	if resolved := ResolveAuthConfig("https://gist.github.com/abc.git", "", ""); resolved.HTTPMethod() != HTTPAuthNone {
		t.Fatalf("gist resolved %+v, want no HTTP credential", resolved)
	}
}

// Entries without their own HTTP(S) credential inherit the global token and user.
func TestResolveAuthConfig_GlobalTokenInheritedByMatchedEntry(t *testing.T) {
	tests := []struct {
		name  string
		entry ScopedAuthConfig
	}{
		{name: "domains only", entry: ScopedAuthConfig{Domains: []string{"gitlab.com"}}},
		{name: "SSH only", entry: ScopedAuthConfig{Domains: []string{"gitlab.com"}, SSHPrivateKey: "scoped-key"}},
		{name: "wildcard", entry: ScopedAuthConfig{Domains: []string{"*.gitlab.com"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ConfigureAuthResolver([]ScopedAuthConfig{tt.entry}, "", "", "global-token", "global-user", GitHubAppConfig{})
			resetAuthResolver(t)

			url := "https://gitlab.com/group/repo.git"
			if tt.name == "wildcard" {
				url = "https://git.gitlab.com/group/repo.git"
			}

			auth, err := GetAuthMethod(url, "", "")
			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}

			basicAuth, ok := auth.(*githttp.BasicAuth)
			if !ok {
				t.Fatalf("expected *githttp.BasicAuth, got %T", auth)
			}

			if basicAuth.Username != "global-user" || basicAuth.Password != "global-token" {
				t.Fatalf("expected global user and token, got %q/%q", basicAuth.Username, basicAuth.Password)
			}
		})
	}
}

func TestResolveAuthConfig_SSHKeyPrecedence(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{
		{Domains: []string{"scoped.example.com"}, SSHPrivateKey: "scoped-key", SSHPrivateKeyPassphrase: "scoped-pass"},
		{Domains: []string{"inherit.example.com"}},
	}, "global-key", "global-pass", "", "", GitHubAppConfig{})
	resetAuthResolver(t)

	tests := []struct {
		url, privateKey, wantKey, wantPass string
	}{
		{url: "git@scoped.example.com:org/repo.git", privateKey: "arg-key", wantKey: "scoped-key", wantPass: "scoped-pass"},
		{url: "git@inherit.example.com:org/repo.git", privateKey: "arg-key", wantKey: "global-key", wantPass: "global-pass"},
		{url: "git@other.example.com:org/repo.git", privateKey: "arg-key", wantKey: "arg-key"},
		{url: "git@other.example.com:org/repo.git", wantKey: "global-key", wantPass: "global-pass"},
	}

	for _, tt := range tests {
		resolved := ResolveAuthConfig(tt.url, tt.privateKey, "")
		if resolved.SSHPrivateKey != tt.wantKey || resolved.SSHPrivateKeyPassphrase != tt.wantPass {
			t.Errorf("ResolveAuthConfig(%q, %q) SSH = %q/%q, want %q/%q", tt.url, tt.privateKey,
				resolved.SSHPrivateKey, resolved.SSHPrivateKeyPassphrase, tt.wantKey, tt.wantPass)
		}
	}
}

func TestGetAuthMethod_UsesScopedHTTPToken(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{
		{
			Domains:        []string{"gitlab.com"},
			GitAccessToken: "scoped-token",
		},
	}, "", "", "", "", GitHubAppConfig{})
	resetAuthResolver(t)

	auth, err := GetAuthMethod("https://gitlab.com/group/repo.git", "", "")
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
	resetAuthResolver(t)

	auth, err := GetAuthMethod("https://gitlab.com/group/repo.git", "", "")
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

func TestGetAuthMethod_RejectsHTTPURLWithoutHost(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{{Domains: []string{"github.com"}}}, "", "", "global-token", "", GitHubAppConfig{})
	resetAuthResolver(t)

	for _, url := range []string{"https:///org/repo.git", "http://:8080/org/repo.git"} {
		auth, err := GetAuthMethod(url, "", "")
		if !errors.Is(err, ErrInvalidRepositoryHost) || auth != nil {
			t.Fatalf("GetAuthMethod(%q) = %v, %v, want ErrInvalidRepositoryHost", url, auth, err)
		}
	}
}

func TestGetAuthMethod_UsesGlobalGitHubAppToken(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{{Domains: []string{"github.com"}}}, "", "", "", "", GitHubAppConfig{
		ID:         "12345",
		PrivateKey: "test-private-key",
	})

	var providerURLs []string

	oldProvider := SwapGitHubAppTokenProviderForTest(func(repoURL string, _ GitHubAppConfig) (string, error) {
		providerURLs = append(providerURLs, repoURL)

		return "ghs-install-token", nil
	})

	t.Cleanup(oldProvider)
	resetAuthResolver(t)

	auth, err := GetAuthMethod("https://github.com/org/repo.git", "", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if auth == nil {
		t.Fatal("expected auth method, got nil")
	}

	if auth.Name() != "http-basic-auth" {
		t.Fatalf("expected http-basic-auth, got '%s'", auth.Name())
	}

	// An unscoped host must not cause a call to the GitHub API with the App key.
	auth, err = GetAuthMethod("https://attacker.example/org/repo.git", "", "")
	if err != nil || auth != nil {
		t.Fatalf("GetAuthMethod() for unscoped host = %v, %v, want anonymous access", auth, err)
	}

	if len(providerURLs) != 1 || providerURLs[0] != "https://github.com/org/repo.git" {
		t.Fatalf("GitHub App provider calls = %v, want only the scoped repository", providerURLs)
	}
}

func TestResolveAuthConfig_ScopedAppBeatsGlobalToken(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{{
		Domains:             []string{"github.com"},
		GitHubAppID:         "99999",
		GitHubAppPrivateKey: "scoped-private-key",
	}}, "", "", "global-token", "", GitHubAppConfig{})
	resetAuthResolver(t)

	resolved := ResolveAuthConfig("https://github.com/org/repo.git", "", "")
	if resolved.HTTPMethod() != HTTPAuthGitHubApp || resolved.GitAccessToken != "" {
		t.Fatalf("expected the scoped GitHub App only, got %+v", resolved)
	}
}

func TestResolveHTTPToken_PrefersEntryToken(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{{
		Domains:        []string{"github.com"},
		GitAccessToken: "entry-token",
	}}, "", "", "", "", GitHubAppConfig{ID: "12345", PrivateKey: "test-private-key"})
	resetAuthResolver(t)

	oldProvider := SwapGitHubAppTokenProviderForTest(func(_ string, _ GitHubAppConfig) (string, error) {
		t.Fatal("github app token provider should not be called when an entry token is set")
		return "", nil
	})
	t.Cleanup(oldProvider)

	url := "https://github.com/org/repo.git"

	token, err := ResolveHTTPToken(url, ResolveAuthConfig(url, "", ""))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if token != "entry-token" {
		t.Fatalf("expected entry token to win, got '%s'", token)
	}
}

func TestResolveHTTPToken_FallsBackToGitHubApp(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{{Domains: []string{"github.com"}}}, "", "", "", "",
		GitHubAppConfig{ID: "12345", PrivateKey: "test-private-key"})
	resetAuthResolver(t)

	oldProvider := SwapGitHubAppTokenProviderForTest(func(_ string, cfg GitHubAppConfig) (string, error) {
		if cfg.ID != "12345" {
			t.Fatalf("expected app id 12345, got %s", cfg.ID)
		}

		return "ghs-install-token", nil // #nosec G101 -- test fixture, not a real credential
	})
	t.Cleanup(oldProvider)

	url := "https://github.com/org/repo.git"

	token, err := ResolveHTTPToken(url, ResolveAuthConfig(url, "", ""))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if token != "ghs-install-token" { // #nosec G101 -- test fixture, not a real credential
		t.Fatalf("expected github app installation token, got '%s'", token)
	}
}

func TestResolveHTTPToken_ReturnsErrorFromGitHubAppProvider(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{{Domains: []string{"github.com"}}}, "", "", "", "",
		GitHubAppConfig{ID: "12345", PrivateKey: "test-private-key"})
	resetAuthResolver(t)

	oldProvider := SwapGitHubAppTokenProviderForTest(func(_ string, _ GitHubAppConfig) (string, error) {
		return "", errors.New("boom")
	})
	t.Cleanup(oldProvider)

	url := "https://github.com/org/repo.git"

	_, err := ResolveHTTPToken(url, ResolveAuthConfig(url, "", ""))
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

// A credential resolved for one host must not be released for another host.
func TestResolveHTTPToken_RejectsHostMismatch(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{{Domains: []string{"github.com"}}}, "", "", "global-token", "", GitHubAppConfig{})
	resetAuthResolver(t)

	resolved := ResolveAuthConfig("git@github.com:org/repo.git", "", "")

	for _, url := range []string{"https://attacker.example/org/repo", "https:///org/repo", ""} {
		token, err := ResolveHTTPToken(url, resolved)
		if !errors.Is(err, ErrCredentialHostMismatch) || token != "" {
			t.Fatalf("ResolveHTTPToken(%q) = %q, %v, want ErrCredentialHostMismatch", url, token, err)
		}
	}

	// A config that was not built by ResolveAuthConfig is not bound to a host.
	token, err := ResolveHTTPToken("https://github.com/org/repo", ResolvedAuthConfig{GitAccessToken: "raw-token"})
	if !errors.Is(err, ErrCredentialHostMismatch) || token != "" {
		t.Fatalf("ResolveHTTPToken() with unbound config = %q, %v, want ErrCredentialHostMismatch", token, err)
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

	t.Cleanup(oldProvider)
	resetAuthResolver(t)

	auth, err := GetAuthMethod("https://github.com/org/repo.git", "", "")
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

func TestGetAuthMethod_GistSkipsGitHubApp(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{{Domains: []string{"gist.github.com"}}}, "", "", "", "", GitHubAppConfig{
		ID:         "12345",
		PrivateKey: "test-private-key",
	})

	oldProvider := SwapGitHubAppTokenProviderForTest(func(repoURL string, _ GitHubAppConfig) (string, error) {
		t.Errorf("github app token provider should not be called for gist URL %s", repoURL)
		return "", errors.New("failed to parse owner/repo from URL")
	})

	t.Cleanup(oldProvider)
	resetAuthResolver(t)

	auth, err := GetAuthMethod("https://gist.github.com/4009787fb1e156c5d0aec4ebfa8d3514.git", "", "")
	if err != nil {
		t.Fatalf("expected anonymous gist access, got error %v", err)
	}

	if auth != nil {
		t.Fatalf("expected no auth method for gist, got %s", auth.Name())
	}
}

func TestGetAuthMethod_GistUsesScopedTokenInsteadOfGitHubApp(t *testing.T) {
	ConfigureAuthResolver([]ScopedAuthConfig{
		{
			Domains:        []string{"gist.github.com"},
			GitAccessToken: "gist-token",
		},
	}, "", "", "", "", GitHubAppConfig{ID: "12345", PrivateKey: "test-private-key"})

	oldProvider := SwapGitHubAppTokenProviderForTest(func(repoURL string, _ GitHubAppConfig) (string, error) {
		t.Errorf("github app token provider should not be called for gist URL %s", repoURL)
		return "", nil
	})

	t.Cleanup(oldProvider)
	resetAuthResolver(t)

	auth, err := GetAuthMethod("https://gist.github.com/4009787fb1e156c5d0aec4ebfa8d3514.git", "", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	basicAuth, ok := auth.(*githttp.BasicAuth)
	if !ok || basicAuth.Password != "gist-token" {
		t.Fatalf("expected scoped gist token auth, got %#v", auth)
	}
}

func TestResolveAuthConfig_DropsGitHubAppOnlyForGists(t *testing.T) {
	app := GitHubAppConfig{ID: "12345", PrivateKey: "test-private-key"}

	ConfigureAuthResolver([]ScopedAuthConfig{
		{
			Domains:             []string{"*.github.com"},
			GitHubAppID:         app.ID,
			GitHubAppPrivateKey: app.PrivateKey,
		},
		{Domains: []string{"github.com", "ghes.example.com", "*.ghes.example.com"}},
	}, "", "", "", "", app)
	resetAuthResolver(t)

	testCases := []struct {
		url     string
		wantApp bool
	}{
		{url: "https://gist.github.com/4009787fb1e156c5d0aec4ebfa8d3514.git", wantApp: false},
		{url: "https://gist.github.com/kimdre/4009787fb1e156c5d0aec4ebfa8d3514", wantApp: false},
		{url: "https://gist.ghes.example.com/4009787fb1e156c5d0aec4ebfa8d3514.git", wantApp: false},
		{url: "https://ghes.example.com/gist/4009787fb1e156c5d0aec4ebfa8d3514.git", wantApp: false},
		{url: "https://github.com/org/repo.git", wantApp: true},
		{url: "https://github.com/gist-owner/repo.git", wantApp: true},
		{url: "https://ghes.example.com/org/gist.git", wantApp: true},
	}

	for _, tc := range testCases {
		got := ResolveAuthConfig(tc.url, "", "").GitHubApp
		if hasApp := got.ID != "" && got.PrivateKey != ""; hasApp != tc.wantApp {
			t.Errorf("ResolveAuthConfig(%q) GitHub App = %+v, want configured: %v", tc.url, got, tc.wantApp)
		}
	}
}

func TestHttpTokenAuth(t *testing.T) {
	testCases := []struct {
		name         string
		username     string
		token        string
		expectNil    bool
		expectedUser string
		expectedErr  error
	}{
		{
			name:         "Valid token defaults username",
			token:        "ghp_test123456",
			expectNil:    false,
			expectedUser: DefaultHTTPAuthUser,
			expectedErr:  nil,
		},
		{
			name:         "Custom username (e.g. GitLab deploy token)",
			username:     "gitlab+deploy-token-123",
			token:        "gldt_test123456",
			expectNil:    false,
			expectedUser: "gitlab+deploy-token-123",
			expectedErr:  nil,
		},
		{
			name:        "Empty token",
			token:       "",
			expectNil:   true,
			expectedErr: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			auth := HttpTokenAuth(tc.username, tc.token)

			if tc.expectNil && auth != nil {
				t.Fatal("Expected nil auth for empty token")
			}

			if !tc.expectNil && auth == nil {
				t.Fatal("Expected non-nil auth for valid token")
			}

			if auth == nil {
				return
			}

			if auth.Name() != "http-basic-auth" {
				t.Fatalf("Expected auth name 'http-basic-auth', got '%s'", auth.Name())
			}

			basicAuth, ok := auth.(*githttp.BasicAuth)
			if !ok {
				t.Fatalf("Expected *githttp.BasicAuth, got %T", auth)
			}

			if basicAuth.Username != tc.expectedUser {
				t.Fatalf("Expected username '%s', got '%s'", tc.expectedUser, basicAuth.Username)
			}
		})
	}
}

func TestSSHAuth(t *testing.T) {
	t.Parallel()

	const (
		encryptedKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABA+Zz/91P
rp2u7NvTWBtLI0AAAAGAAAAAEAAAAzAAAAC3NzaC1lZDI1NTE5AAAAIFyEIiKcYAJl82Ga
40hVJoKO1qOvVfekORkGLSsKFnF7AAAAoBgOn6fvoLqNvcj0QMyuZTYVJEm9YXs8zNkG+9
suGsdNHOvMRQWLzq9VJiJUyOG29zayIQ4Q3pZlcoRINpUI9yl4/eFza7P4MEHDVBLF531K
X3nAnZomTg2czfus92AmR+3kYDWvBE1WkpieAaRfVTuBtNcB41rOAZMLQ001zhVF2qdb+D
+tvLTkrbIyLPEbZOBHuCH+mVgPefYCRXsB9Nw=
-----END OPENSSH PRIVATE KEY-----`
		encryptedKeyPassphrase = "doco-cd"
		unencryptedKey         = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACCU6Sk58h0kd2bUvHHvyS1JQiLgBf6yKaIbpGlK8TEfVAAAAJgBQMSpAUDE
qQAAAAtzc2gtZWQyNTUxOQAAACCU6Sk58h0kd2bUvHHvyS1JQiLgBf6yKaIbpGlK8TEfVA
AAAEBBVspZHjWj6Np5szQQHB6w+1X3ZOatDcMmcnm1+R9J9pTpKTnyHSR3ZtS8ce/JLUlC
IuAF/rIpohukaUrxMR9UAAAADmtpbUBraW0tZmVkb3JhAQIDBAUGBw==
-----END OPENSSH PRIVATE KEY-----`
	)

	testCases := []struct {
		name        string
		privateKey  string
		passphrase  string
		expectedErr string
	}{
		{
			name:        "Encrypted ED25519 key",
			privateKey:  encryptedKey,
			passphrase:  encryptedKeyPassphrase,
			expectedErr: "",
		},
		{
			name:        "Missing passphrase for encrypted key",
			privateKey:  encryptedKey,
			passphrase:  "",
			expectedErr: "failed to create SSH public keys: bcrypt_pbkdf: empty password",
		},
		{
			name:        "Unencrypted ED25519 key",
			privateKey:  unencryptedKey,
			passphrase:  "",
			expectedErr: "",
		},
		{
			name:        "Unencrypted ED25519 key with passphrase",
			privateKey:  unencryptedKey,
			passphrase:  "test",
			expectedErr: "",
		},
		{
			name:        "Missing private key",
			privateKey:  "",
			passphrase:  "",
			expectedErr: "ssh URL requires SSH_PRIVATE_KEY to be set",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			auth, err := SSHAuth(tc.privateKey, tc.passphrase)
			if err != nil {
				if tc.expectedErr == "" {
					t.Fatalf("Expected no error, got %v", err)
				}

				if err.Error() == tc.expectedErr {
					return
				}

				t.Fatalf("Expected error %v, got %v", tc.expectedErr, err.Error())
			} else if tc.expectedErr != "" {
				t.Fatalf("Expected error %v, got none", tc.expectedErr)
			}

			if auth == nil {
				if tc.expectedErr != "auth empty" {
					t.Fatal("Expected auth to be non-nil")
				}
			}

			if auth.Name() != "ssh-public-keys" {
				t.Fatalf("Expected auth name 'ssh-public-keys', got '%s'", auth.Name())
			}
		})
	}
}
