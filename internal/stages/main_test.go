package stages

import (
	"os"
	"testing"

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

func TestMain(m *testing.M) {
	configureTestGitAuth()
	os.Exit(m.Run())
}
