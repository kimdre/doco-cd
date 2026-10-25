package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/git"
)

// testGitAccessToken is the global token that commit status tests use.
// GIT_AUTH_DOMAINS scopes it to github.com and to the local test servers.
const testGitAccessToken = "test-token" // #nosec G101 -- test fixture, not a real credential

// configureTestGitAuth sets the default Git credentials for the tests of this package.
// Tests that change the global resolver restore this state, because parallel tests run afterwards.
func configureTestGitAuth() {
	git.ConfigureAuthResolver(
		[]git.ScopedAuthConfig{{Domains: []string{"github.com", "127.0.0.1"}}},
		"", "", testGitAccessToken, "", git.GitHubAppConfig{},
	)
}

// useConfiguredGitAuth applies the Git credentials of cfg, for example from the environment.
// The default test credentials come back when the test ends.
func useConfiguredGitAuth(t *testing.T, cfg *app.Config) {
	t.Helper()

	cfg.ConfigureGitAuth()
	t.Cleanup(configureTestGitAuth)
}

func TestConfigureGitAuth(t *testing.T) {
	t.Cleanup(configureTestGitAuth)

	tests := []struct {
		name         string
		config       app.Config
		wantEnvNames string
	}{
		{
			name:   "strict mode",
			config: app.Config{GitAccessToken: testGitAccessToken},
		},
		{
			name:   "no global credential",
			config: app.Config{GitAuthAllowAnyHost: true},
		},
		{
			name: "scoped credential only",
			config: app.Config{
				GitAuthAllowAnyHost: true,
				GitAuthDomains: []git.ScopedAuthConfig{{
					Domains: []string{"github.com"}, GitAccessToken: testGitAccessToken,
				}},
			},
		},
		{
			name: "global token",
			config: app.Config{
				GitAuthAllowAnyHost: true, GitAccessToken: testGitAccessToken,
			},
			wantEnvNames: "GIT_ACCESS_TOKEN",
		},
		{
			name: "global GitHub App",
			config: app.Config{
				GitAuthAllowAnyHost: true, GitHubAppID: "12345", GitHubAppPrivateKey: "fixture-private-key",
			},
			wantEnvNames: "GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			configureGitAuth(slog.New(slog.NewTextHandler(&out, nil)), &tt.config)

			got := out.String()
			if tt.wantEnvNames == "" {
				if got != "" {
					t.Fatalf("unexpected warning: %s", got)
				}

				return
			}

			for _, want := range []string{"level=WARN", "GIT_AUTH_ALLOW_ANY_HOST", tt.wantEnvNames} {
				if !strings.Contains(got, want) {
					t.Errorf("warning %q does not contain %q", got, want)
				}
			}

			for _, secret := range []string{testGitAccessToken, "fixture-private-key"} {
				if strings.Contains(got, secret) {
					t.Fatal("warning contains a credential value")
				}
			}
		})
	}
}
