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
			name: "Valid config",
			config: Config{
				Source:    config.SourceTypeGit,
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  10,
			},
			expected: nil,
		},
		{
			name: "Valid OCI config",
			config: Config{
				Source:    config.SourceTypeOCI,
				Artifact:  "ghcr.io/example/app-config:main",
				Layout:    config.OciArtifactLayoutV1,
				Reference: "main",
				Interval:  10,
			},
			expected: nil,
		},
		{
			name: "Invalid config - empty CloneUrl",
			config: Config{
				Source:    config.SourceTypeGit,
				CloneUrl:  "",
				Reference: "main",
				Interval:  10,
			},
			expected: deploy.ErrKeyNotFound,
		},
		{
			name: "Invalid config - empty Reference",
			config: Config{
				Source:    config.SourceTypeGit,
				CloneUrl:  "https://example.com/repo.git",
				Reference: "",
				Interval:  10,
			},
			expected: deploy.ErrKeyNotFound,
		},
		{
			name: "Invalid config - OCI missing artifact",
			config: Config{
				Source:    config.SourceTypeOCI,
				Artifact:  "",
				Reference: "main",
				Interval:  10,
			},
			expected: deploy.ErrKeyNotFound,
		},
		{
			name: "Invalid config - OCI unsupported layout",
			config: Config{
				Source:    config.SourceTypeOCI,
				Artifact:  "ghcr.io/example/app-config:main",
				Layout:    "doco.v2",
				Reference: "main",
				Interval:  10,
			},
			expected: ErrInvalidConfig,
		},
		{
			name: "Invalid config - negative Interval",
			config: Config{
				Source:    config.SourceTypeGit,
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  -5,
			},
			expected: ErrIntervalTooLow,
		},
		{
			name: "Invalid config - 5s Interval",
			config: Config{
				Source:    config.SourceTypeGit,
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  5,
			},
			expected: ErrIntervalTooLow,
		},
		{
			name: "Invalid config - zero Interval (Disabled)",
			config: Config{
				Source:    config.SourceTypeGit,
				CloneUrl:  "https://example.com/repo.git",
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
			name: "Valid config",
			config: Config{
				Source:    config.SourceTypeGit,
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  10,
			},
			expected: "Config{Source: git, CloneUrl: https://example.com/repo.git, Reference: main, Interval: 10}",
		},
		{
			name: "OCI config",
			config: Config{
				Source:    config.SourceTypeOCI,
				Artifact:  "ghcr.io/example/app-config:main",
				Layout:    config.OciArtifactLayoutV1,
				Reference: "main",
				Interval:  10,
			},
			expected: "Config{Source: oci, Artifact: ghcr.io/example/app-config:main, Layout: doco.v1, Reference: main, Interval: 10}",
		},
		{
			name: "Basic config",
			config: Config{
				Source:    config.SourceTypeGit,
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  180,
			},
			expected: "Config{Source: git, CloneUrl: https://example.com/repo.git, Reference: main, Interval: 180}",
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
