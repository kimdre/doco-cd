package poll

import (
	"errors"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
)

func TestConfig_Validate(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		config   Config
		expected error
	}{
		{
			name: "Valid git config",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "https://example.com/repo.git",
				Reference: "main",
				Interval:  10,
			},
			expected: nil,
		},
		{
			name: "Valid git config - SSH scp-style URL",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "git@github.com:owner/repo.git",
				Reference: "main",
				Interval:  10,
			},
			expected: nil,
		},
		{
			name: "Valid OCI config",
			config: Config{
				Source:    config.SourceTypeOCI,
				SourceUrl: "ghcr.io/example/app-config:main",
				Interval:  10,
			},
			expected: nil,
		},
		{
			name: "Valid OCI config - tagged reference",
			config: Config{
				Source:    config.SourceTypeOCI,
				SourceUrl: "ghcr.io/example/app-config:v1.0.0",
				Interval:  10,
			},
			expected: nil,
		},
		{
			name: "Invalid config - empty SourceUrl for git",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "",
				Reference: "main",
				Interval:  10,
			},
			expected: deploy.ErrKeyNotFound,
		},
		{
			name: "Invalid config - empty Reference",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "https://example.com/repo.git",
				Reference: "",
				Interval:  10,
			},
			expected: deploy.ErrKeyNotFound,
		},
		{
			name: "Invalid config - empty SourceUrl for OCI",
			config: Config{
				Source:    config.SourceTypeOCI,
				SourceUrl: "",
				Interval:  10,
			},
			expected: deploy.ErrKeyNotFound,
		},
		{
			name: "Invalid config - invalid git URL scheme",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "ftp://example.com/repo.git",
				Reference: "main",
				Interval:  10,
			},
			expected: ErrInvalidConfig,
		},
		{
			name: "Invalid config - negative Interval",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "https://example.com/repo.git",
				Reference: "main",
				Interval:  -5,
			},
			expected: ErrIntervalTooLow,
		},
		{
			name: "Invalid config - 5s Interval",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "https://example.com/repo.git",
				Reference: "main",
				Interval:  5,
			},
			expected: ErrIntervalTooLow,
		},
		{
			name: "Valid config - zero Interval (disabled)",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "https://example.com/repo.git",
				Reference: "main",
				Interval:  0,
			},
			expected: nil,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.config.Validate()
			if !errors.Is(err, tc.expected) {
				t.Errorf("expected %v, got %v", tc.expected, err)
			}
		})
	}
}

func TestConfig_String(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		config   Config
		expected string
	}{
		{
			name: "Git config",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "https://example.com/repo.git",
				Reference: "main",
				Interval:  10,
			},
			expected: "Config{Source: git, SourceUrl: https://example.com/repo.git, Reference: main, Interval: 10}",
		},
		{
			name: "OCI config",
			config: Config{
				Source:    config.SourceTypeOCI,
				SourceUrl: "ghcr.io/example/app-config:main",
				Reference: "main",
				Interval:  10,
			},
			expected: "Config{Source: oci, SourceUrl: ghcr.io/example/app-config:main, Reference: main, Interval: 10}",
		},
		{
			name: "Basic config",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "https://example.com/repo.git",
				Reference: "main",
				Interval:  180,
			},
			expected: "Config{Source: git, SourceUrl: https://example.com/repo.git, Reference: main, Interval: 180}",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := tc.config.String()
			if result != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, result)
			}
		})
	}
}

func TestOCIConfig_ReferenceAutoderived(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		artifact string
		wantRef  string
	}{
		{"ghcr.io/myorg/config:main", "main"},
		{"ghcr.io/myorg/config:v1.2.3", "v1.2.3"},
		{"ghcr.io/myorg/config:production", "production"},
	}

	for _, tc := range testCases {
		t.Run(tc.artifact, func(t *testing.T) {
			t.Parallel()

			c := Config{
				Source:    config.SourceTypeOCI,
				SourceUrl: tc.artifact,
				Interval:  10,
			}

			if err := c.Validate(); err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}

			if c.Reference != tc.wantRef {
				t.Errorf("expected reference %q, got %q", tc.wantRef, c.Reference)
			}
		})
	}
}
