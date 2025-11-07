package config

import (
	"errors"
	"testing"
)

func TestPollConfig_Validate(t *testing.T) {
	testCases := []struct {
		name     string
		config   PollConfig
		expected error
	}{
		{
			name: "Valid config",
			config: PollConfig{
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  10,
			},
			expected: nil,
		},
		{
			name: "Invalid config - empty CloneUrl",
			config: PollConfig{
				CloneUrl:  "",
				Reference: "main",
				Interval:  10,
			},
			expected: ErrKeyNotFound,
		},
		{
			name: "Invalid config - empty Reference",
			config: PollConfig{
				CloneUrl:  "https://example.com/repo.git",
				Reference: "",
				Interval:  10,
			},
			expected: ErrKeyNotFound,
		},
		{
			name: "Invalid config - negative Interval",
			config: PollConfig{
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  -5,
			},
			expected: ErrInvalidPollConfig,
		},
		{
			name: "Invalid config - 5s Interval",
			config: PollConfig{
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  5,
			},
			expected: ErrInvalidPollConfig,
		},
		{
			name: "Invalid config - zero Interval (Disabled)",
			config: PollConfig{
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  0,
			},
			expected: nil,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.Validate()
			if !errors.Is(err, tc.expected) {
				t.Errorf("expected %v, got %v", tc.expected, err)
			}
		})
	}
}

func TestPollConfig_String(t *testing.T) {
	testCases := []struct {
		name     string
		config   PollConfig
		expected string
	}{
		{
			name: "Valid config",
			config: PollConfig{
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  10,
			},
			expected: "PollConfig{CloneUrl: https://example.com/repo.git, Reference: main, Interval: 10}",
		},
		{
			name: "Basic config",
			config: PollConfig{
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  180,
			},
			expected: "PollConfig{CloneUrl: https://example.com/repo.git, Reference: main, Interval: 180}",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.config.String()
			if result != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, result)
			}
		})
	}
}

func TestPollConfig_ValidateInlineDeployments(t *testing.T) {
	pollConfig := PollConfig{
		CloneUrl:  "https://example.com/repo.git",
		Reference: "main",
		Deployments: []*DeployConfig{
			{
				Name:         "app",
				ComposeFiles: []string{"compose.yaml"},
			},
		},
	}

	if err := pollConfig.Validate(); err != nil {
		t.Fatalf("expected inline deployments to validate: %v", err)
	}

	if pollConfig.Deployments[0].Reference != pollConfig.Reference {
		t.Fatalf("expected inline deployment reference to default to poll reference, got %s", pollConfig.Deployments[0].Reference)
	}
}

func TestPollConfig_ValidateInlineDeploymentsDuplicateNames(t *testing.T) {
	pollConfig := PollConfig{
		CloneUrl:  "https://example.com/repo.git",
		Reference: "main",
		Deployments: []*DeployConfig{
			{
				Name:         "app",
				ComposeFiles: []string{"compose.yaml"},
			},
			{
				Name:         "app",
				ComposeFiles: []string{"compose.yaml"},
			},
		},
	}

	err := pollConfig.Validate()
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig when duplicate inline deployment names are provided, got %v", err)
	}
}
