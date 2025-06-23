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
				Private:   true,
			},
			expected: nil,
		},
		{
			name: "Invalid config - empty CloneUrl",
			config: PollConfig{
				CloneUrl:  "",
				Reference: "main",
				Interval:  10,
				Private:   true,
			},
			expected: ErrKeyNotFound,
		},
		{
			name: "Invalid config - empty Reference",
			config: PollConfig{
				CloneUrl:  "https://example.com/repo.git",
				Reference: "",
				Interval:  10,
				Private:   true,
			},
			expected: ErrKeyNotFound,
		},
		{
			name: "Invalid config - negative Interval",
			config: PollConfig{
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  -5,
				Private:   true,
			},
			expected: ErrInvalidPollConfig,
		},
		{
			name: "Invalid config - 5s Interval",
			config: PollConfig{
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  5,
				Private:   true,
			},
			expected: ErrInvalidPollConfig,
		},
		{
			name: "Invalid config - zero Interval (Disabled)",
			config: PollConfig{
				CloneUrl:  "https://example.com/repo.git",
				Reference: "main",
				Interval:  0,
				Private:   true,
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
				Private:   true,
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
