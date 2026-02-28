package stages

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
)

func TestGetFullName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cloneURL string
		expected string
	}{
		{
			cloneURL: "https://github.com/kimdre/doco-cd_tests.git",
			expected: "kimdre/doco-cd_tests",
		},
		{
			cloneURL: "https://user:password@github.com/kimdre/doco-cd_tests.git", // #nosec G101 -- This is a test URL, not a real token
			expected: "kimdre/doco-cd_tests",
		},
		{
			cloneURL: "http://git.example.com/doco-cd.git",
			expected: "doco-cd",
		},
		// SSH SCP-like
		{
			cloneURL: "git@github.com:kimdre/doco-cd_tests.git",
			expected: "kimdre/doco-cd_tests",
		},
		// SSH URL
		{
			cloneURL: "ssh://git@github.com/kimdre/doco-cd_tests.git",
			expected: "kimdre/doco-cd_tests",
		},
		{
			cloneURL: "ssh://github.com/kimdre/doco-cd_tests.git",
			expected: "kimdre/doco-cd_tests",
		},
		// Token-injected HTTPS
		{
			cloneURL: "https://oauth2:TOKEN@github.com/kimdre/doco-cd_tests.git", // #nosec G101 -- This is a test URL, not a real token
			expected: "kimdre/doco-cd_tests",
		},
	}
	for _, tt := range tests {
		t.Run(tt.cloneURL, func(t *testing.T) {
			t.Parallel()

			result := getFullName(config.HttpUrl(tt.cloneURL))
			if result != tt.expected {
				t.Errorf("getFullName failed for %s: expected %s, got %s", tt.cloneURL, tt.expected, result)
			}
		})
	}
}
