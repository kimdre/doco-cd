package config

import (
	"errors"
	"os"
	"testing"
)

func TestGetAppConfig(t *testing.T) {
	// Set up test cases
	tests := []struct {
		name        string
		envVars     map[string]string
		expectedErr error
	}{
		{
			name: "valid config",
			envVars: map[string]string{
				"LOG_LEVEL":             "info",
				"HTTP_PORT":             "8080",
				"WEBHOOK_SECRET":        "secret",
				"AUTH_TYPE":             "oauth2",
				"DOCKER_API_VERSION":    "v1.40",
				"GIT_ACCESS_TOKEN":      "token",
				"SKIP_TLS_VERIFICATION": "false",
			},
			expectedErr: nil,
		},
		{
			name: "invalid log level",
			envVars: map[string]string{
				"LOG_LEVEL":      "invalid",
				"WEBHOOK_SECRET": "secret",
			},
			expectedErr: ErrInvalidLogLevel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up the environment
			for k, v := range tt.envVars {
				if err := os.Setenv(k, v); err != nil {
					t.Fatalf("failed to set environment variable: %v", err)
				}
			}

			// Run the test
			_, err := GetAppConfig()
			if !errors.Is(err, tt.expectedErr) {
				t.Errorf("expected error to be '%v', got '%v'", tt.expectedErr, err)
			}

			if err == nil {
				// Clean up the environment
				for k := range tt.envVars {
					if err := os.Unsetenv(k); err != nil {
						t.Fatalf("failed to unset environment variable: %v", err)
					}
				}
			}
		})
	}
}
