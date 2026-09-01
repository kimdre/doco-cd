package poll

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
)

func TestConfig_Validate(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		config   Config
		expected error
		wantRef  string
		wantURL  string
	}{
		{
			name: "Valid git config",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "https://example.com/repo.git",
				Reference: "main",
				Interval:  10 * time.Second,
			},
			expected: nil,
			wantRef:  "main",
		},
		{
			name: "Valid git config - SSH scp-style URL",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "git@github.com:owner/repo.git",
				Reference: "main",
				Interval:  10 * time.Second,
			},
			expected: nil,
			wantRef:  "main",
		},
		{
			name: "Valid git config - local filesystem file:// URL",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "file:///data/local-repos/my-app",
				Reference: "main",
				Interval:  10 * time.Second,
			},
			expected: nil,
			wantRef:  "main",
			wantURL:  "file:///data/local-repos/my-app",
		},
		{
			name: "Valid git config - local filesystem absolute path",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "/data/local-repos/my-app",
				Reference: "main",
				Interval:  10 * time.Second,
			},
			expected: nil,
			wantRef:  "main",
			wantURL:  "file:///data/local-repos/my-app",
		},
		{
			name: "Valid OCI config",
			config: Config{
				Source:    config.SourceTypeOCI,
				SourceUrl: "ghcr.io/example/app-config:main",
				Interval:  10 * time.Second,
			},
			expected: nil,
			wantRef:  "main",
		},
		{
			name: "Valid OCI config - tagged reference",
			config: Config{
				Source:    config.SourceTypeOCI,
				SourceUrl: "ghcr.io/example/app-config:v1.0.0",
				Interval:  10 * time.Second,
			},
			expected: nil,
			wantRef:  "v1.0.0",
		},
		{
			name: "Invalid config - empty SourceUrl for git",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "",
				Reference: "main",
				Interval:  10 * time.Second,
			},
			expected: deploy.ErrKeyNotFound,
		},
		{
			name: "Valid git config - empty Reference defaults to main",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "https://example.com/repo.git",
				Reference: "",
				Interval:  10 * time.Second,
			},
			expected: nil,
			wantRef:  "refs/heads/main",
		},
		{
			name: "Invalid config - empty SourceUrl for OCI",
			config: Config{
				Source:    config.SourceTypeOCI,
				SourceUrl: "",
				Interval:  10 * time.Second,
			},
			expected: deploy.ErrKeyNotFound,
		},
		{
			name: "Invalid config - invalid git URL scheme",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "ftp://example.com/repo.git",
				Reference: "main",
				Interval:  10 * time.Second,
			},
			expected: ErrInvalidConfig,
			wantRef:  "main",
		},
		{
			name: "Invalid config - negative Interval",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "https://example.com/repo.git",
				Reference: "main",
				Interval:  -5 * time.Second,
			},
			expected: ErrIntervalTooLow,
			wantRef:  "main",
		},
		{
			name: "Invalid config - 5s Interval",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "https://example.com/repo.git",
				Reference: "main",
				Interval:  5 * time.Second,
			},
			expected: ErrIntervalTooLow,
			wantRef:  "main",
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
			wantRef:  "main",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.config.Validate()
			if !errors.Is(err, tc.expected) {
				t.Errorf("expected %v, got %v", tc.expected, err)
				return
			}

			if tc.wantRef != "" && tc.config.Reference != tc.wantRef {
				t.Errorf("expected reference %q, got %q", tc.wantRef, tc.config.Reference)
			}

			if tc.wantURL != "" && tc.config.SourceUrl != tc.wantURL {
				t.Errorf("expected source URL %q, got %q", tc.wantURL, tc.config.SourceUrl)
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
				Interval:  10 * time.Second,
			},
			expected: "Config{Source: git, SourceUrl: https://example.com/repo.git, Reference: main, Interval: 10s}",
		},
		{
			name: "OCI config",
			config: Config{
				Source:    config.SourceTypeOCI,
				SourceUrl: "ghcr.io/example/app-config:main",
				Reference: "main",
				Interval:  10 * time.Second,
			},
			expected: "Config{Source: oci, SourceUrl: ghcr.io/example/app-config:main, Reference: main, Interval: 10s}",
		},
		{
			name: "Basic config",
			config: Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "https://example.com/repo.git",
				Reference: "main",
				Interval:  180 * time.Second,
			},
			expected: "Config{Source: git, SourceUrl: https://example.com/repo.git, Reference: main, Interval: 3m0s}",
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

func TestParsePollInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    any
		expected time.Duration
		wantErr  bool
	}{
		{name: "int", input: 300, expected: 300 * time.Second},
		{name: "float64 whole number", input: float64(90), expected: 90 * time.Second},
		{name: "numeric string", input: "300", expected: 300 * time.Second},
		{name: "duration string", input: "5m", expected: 5 * time.Minute},
		{name: "composite duration string", input: "1m30s", expected: 90 * time.Second},
		{name: "zero duration", input: "0s", expected: 0},
		{name: "fractional seconds duration", input: "500ms", wantErr: true},
		{name: "invalid duration", input: "abc", wantErr: true},
		{name: "invalid type", input: true, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parsePollInterval(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tc.expected {
				t.Fatalf("expected %s, got %s", tc.expected, got)
			}
		})
	}
}

func TestConfig_UnmarshalYAML_IntervalDuration(t *testing.T) {
	t.Parallel()

	raw := []byte(`
- source: git
  url: https://example.com/repo.git
  reference: main
  interval: 5m
`)

	var cfg []Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("failed to unmarshal yaml: %v", err)
	}

	if len(cfg) != 1 {
		t.Fatalf("expected 1 config, got %d", len(cfg))
	}

	if cfg[0].Interval != 5*time.Minute {
		t.Fatalf("expected interval to normalize to 5m0s, got %s", cfg[0].Interval)
	}
}

func TestConfig_UnmarshalYAML_IntervalDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		intervalLine string
		want         time.Duration
	}{
		{name: "omitted", want: 3 * time.Minute},
		{name: "explicit zero", intervalLine: "interval: 0\n", want: 0},
		{name: "explicit null", intervalLine: "interval: null\n", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			raw := "source: oci\nurl: ghcr.io/example/app:test\n" + tt.intervalLine
			if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
				t.Fatalf("failed to unmarshal yaml: %v", err)
			}

			if cfg.Interval != tt.want {
				t.Fatalf("interval = %s, want %s", cfg.Interval, tt.want)
			}
		})
	}
}

func TestConfig_UnmarshalYAML_WatchDefaultsToTrueAndCanBeDisabled(t *testing.T) {
	t.Parallel()

	raw := []byte(`
- source: git
  url: /local-repos/my-app
  reference: main
- source: git
  url: /local-repos/other-app
  reference: main
  watch: false
`)

	var cfg []Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("failed to unmarshal yaml: %v", err)
	}

	if len(cfg) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(cfg))
	}

	if !cfg[0].Watch {
		t.Fatal("expected watch to default to true when unset")
	}

	if cfg[1].Watch {
		t.Fatal("expected watch to be false when explicitly disabled")
	}
}

func TestConfig_UnmarshalJSON_IntervalVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected time.Duration
		watch    bool
	}{
		{
			name:     "duration string",
			input:    `{"source":"git","url":"https://example.com/repo.git","reference":"main","interval":"1m30s"}`,
			expected: 90 * time.Second,
			watch:    true,
		},
		{
			name:     "numeric string treated as seconds",
			input:    `{"source":"git","url":"https://example.com/repo.git","reference":"main","interval":"300"}`,
			expected: 300 * time.Second,
			watch:    true,
		},
		{
			name:     "integer seconds",
			input:    `{"source":"git","url":"https://example.com/repo.git","reference":"main","interval":45}`,
			expected: 45 * time.Second,
			watch:    true,
		},
		{
			name:     "explicit zero",
			input:    `{"source":"oci","url":"ghcr.io/example/app:test","interval":0}`,
			expected: 0,
			watch:    true,
		},
		{
			name:     "explicit null",
			input:    `{"source":"oci","url":"ghcr.io/example/app:test","interval":null}`,
			expected: 0,
			watch:    true,
		},
		{
			name:     "omitted interval",
			input:    `{"source":"oci","url":"ghcr.io/example/app:test"}`,
			expected: 3 * time.Minute,
			watch:    true,
		},
		{
			name:     "watch disabled",
			input:    `{"source":"git","url":"https://example.com/repo.git","reference":"main","interval":45,"watch":false}`,
			expected: 45 * time.Second,
			watch:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var cfg Config
			if err := json.Unmarshal([]byte(tc.input), &cfg); err != nil {
				t.Fatalf("failed to unmarshal json: %v", err)
			}

			if cfg.Interval != tc.expected {
				t.Fatalf("expected interval %s, got %s", tc.expected, cfg.Interval)
			}

			if cfg.Watch != tc.watch {
				t.Fatalf("expected watch=%t, got %t", tc.watch, cfg.Watch)
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
				Interval:  10 * time.Second,
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
