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
