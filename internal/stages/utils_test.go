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
	}
	for _, tt := range tests {
		result := GetRepoName(tt.cloneURL)
		if result != tt.expected {
			t.Errorf("GetRepoName failed for %s: expected %s, got %s", tt.cloneURL, tt.expected, result)
		}
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
	}
	for _, tt := range tests {
		result := getFullName(config.HttpUrl(tt.cloneURL))
		if result != tt.expected {
			t.Errorf("getFullName failed for %s: expected %s, got %s", tt.cloneURL, tt.expected, result)
		}
	}
}
