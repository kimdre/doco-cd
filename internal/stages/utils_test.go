package stages

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
)

func TestGetRepoName(t *testing.T) {
	tests := []struct {
		cloneURL string
		expected string
	}{
		{
			cloneURL: "https://github.com/kimdre/doco-cd_tests.git",
			expected: "github.com/kimdre/doco-cd_tests",
		},
		{
			cloneURL: "https://user:password@github.com/kimdre/doco-cd_tests.git",
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
			cloneURL: "https://oauth2:TOKEN@github.com/kimdre/doco-cd_tests.git",
			expected: "github.com/kimdre/doco-cd_tests",
		},
	}
	for _, tt := range tests {
		t.Run(tt.cloneURL, func(t *testing.T) {
			result := GetRepoName(tt.cloneURL)
			if result != tt.expected {
				t.Errorf("GetRepoName failed for %s: expected %s, got %s", tt.cloneURL, tt.expected, result)
			}
		})
	}
}

func TestGetFullName(t *testing.T) {
	tests := []struct {
		cloneURL string
		expected string
	}{
		{
			cloneURL: "https://github.com/kimdre/doco-cd_tests.git",
			expected: "kimdre/doco-cd_tests",
		},
		{
			cloneURL: "https://user:password@github.com/kimdre/doco-cd_tests.git",
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
			cloneURL: "https://oauth2:TOKEN@github.com/kimdre/doco-cd_tests.git",
			expected: "kimdre/doco-cd_tests",
		},
	}
	for _, tt := range tests {
		t.Run(tt.cloneURL, func(t *testing.T) {
			result := getFullName(config.HttpUrl(tt.cloneURL))
			if result != tt.expected {
				t.Errorf("getFullName failed for %s: expected %s, got %s", tt.cloneURL, tt.expected, result)
			}
		})
	}
}
