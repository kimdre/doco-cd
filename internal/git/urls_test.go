package git_test

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/git"
)

func TestConvertSSHUrl(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		sshUrl   string
		expected string
	}{
		{
			name:     "Valid SSH URL",
			sshUrl:   "git@github.com:user/repo.git",
			expected: "ssh://git@github.com/user/repo.git",
		},
		{
			name:     "Valid SSH URL without .git",
			sshUrl:   "git@github.com:user/repo",
			expected: "ssh://git@github.com/user/repo",
		},
		{
			name:     "SSH URL with non-default port stays unchanged",
			sshUrl:   "ssh://git@github.com:2222/user/repo.git",
			expected: "ssh://git@github.com:2222/user/repo.git",
		},
		{
			name:     "SSH URL with ",
			sshUrl:   "ssh://git@gitea:2222/user/repo.git",
			expected: "ssh://git@gitea:2222/user/repo.git",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := git.ConvertSSHUrl(tc.sshUrl)
			if tc.expected == "" {
				if result != tc.expected {
					t.Fatalf("Expected empty string for invalid URL, got %s", result)
				}
			}

			if result != tc.expected {
				t.Fatalf("Expected %s, got %s", tc.expected, result)
			}
		})
	}
}

func TestGetRepoName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cloneURL string
		expected string
	}{
		{
			cloneURL: "https://github.com/kimdre/doco-cd_tests.git",
			expected: "github.com/kimdre/doco-cd_tests",
		},
		{
			cloneURL: "https://user:password@github.com/kimdre/doco-cd_tests.git", // #nosec G101 -- This is a test URL, not a real token
			expected: "github.com/kimdre/doco-cd_tests",
		},
		{
			cloneURL: "http://git.example.com/doco-cd.git",
			expected: "git.example.com/doco-cd",
		},
		// SSH SCP-like
		{
			cloneURL: "git@github.com:kimdre/doco-cd_tests.git",
			expected: "github.com/kimdre/doco-cd_tests",
		},
		// SSH URL
		{
			cloneURL: "ssh://git@github.com/kimdre/doco-cd_tests.git",
			expected: "github.com/kimdre/doco-cd_tests",
		},
		{
			cloneURL: "ssh://github.com/kimdre/doco-cd_tests.git",
			expected: "github.com/kimdre/doco-cd_tests",
		},
		// Token-injected HTTPS
		{
			cloneURL: "https://oauth2:TOKEN@github.com/kimdre/doco-cd_tests.git", // #nosec G101 -- This is a test URL, not a real token
			expected: "github.com/kimdre/doco-cd_tests",
		},
		{
			cloneURL: "http://git.example.com/infra/alpha/local/netbird-doco.git",
			expected: "git.example.com/infra/alpha/local/netbird-doco",
		},
		{
			cloneURL: "git@gitlab.com:gitlab-org/5-minute-production-app/sandbox/cats.git",
			expected: "gitlab.com/gitlab-org/5-minute-production-app/sandbox/cats",
		},
		{
			cloneURL: "https://gitlab.com/gitlab-org/5-minute-production-app/sandbox/cats.git",
			expected: "gitlab.com/gitlab-org/5-minute-production-app/sandbox/cats",
		},
		// Local filesystem repositories (file:// URLs)
		{
			cloneURL: "file:///data/local-repos/my-app",
			expected: "data/local-repos/my-app",
		},
		{
			cloneURL: "file:///data/local-repos/my-app.git",
			expected: "data/local-repos/my-app",
		},
		{
			cloneURL: "file:///my-app",
			expected: "my-app",
		},
	}
	for _, tt := range tests {
		t.Run(tt.cloneURL, func(t *testing.T) {
			result := git.GetRepoName(tt.cloneURL)
			if result != tt.expected {
				t.Errorf("GetRepoName failed for %s: expected %s, got %s", tt.cloneURL, tt.expected, result)
			}
		})
	}
}
