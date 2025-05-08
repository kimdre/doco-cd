package config

import (
	"errors"
	"os"
	"path"
	"strconv"
	"testing"
)

func TestGetAppConfig(t *testing.T) {
	// Set up test cases
	tests := []struct {
		name          string
		envVars       map[string]string
		dockerSecrets map[string]string
		expectedErr   error
	}{
		{
			name: "valid config",
			envVars: map[string]string{
				"LOG_LEVEL":             "info",
				"HTTP_PORT":             "8080",
				"WEBHOOK_SECRET":        "secret",
				"AUTH_TYPE":             "oauth2",
				"GIT_ACCESS_TOKEN":      "token",
				"SKIP_TLS_VERIFICATION": "false",
			},
			dockerSecrets: nil,
			expectedErr:   nil,
		},
		{
			name: "invalid log level",
			envVars: map[string]string{
				"LOG_LEVEL":      "invalid",
				"WEBHOOK_SECRET": "secret",
			},
			dockerSecrets: nil,
			expectedErr:   ErrInvalidLogLevel,
		},
		{
			name: "valid config with docker secrets",
			envVars: map[string]string{
				"LOG_LEVEL":             "info",
				"HTTP_PORT":             "8080",
				"AUTH_TYPE":             "oauth2",
				"SKIP_TLS_VERIFICATION": "false",
			},
			dockerSecrets: map[string]string{
				"WEBHOOK_SECRET":   "secret",
				"GIT_ACCESS_TOKEN": "token",
			},
			expectedErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secretsPath := path.Join(t.TempDir(), DockerSecretsPath)

			// Create the Docker secrets directory
			if err := os.MkdirAll(secretsPath, 0o755); err != nil {
				t.Fatalf("failed to create Docker secrets directory: %v", err)
			}

			t.Cleanup(func() {
				// Clean up the environment
				for k := range tt.envVars {
					if err := os.Unsetenv(k); err != nil {
						t.Fatalf("failed to unset environment variable: %v", err)
					}
				}
			})

			// Set up Docker secrets
			for k, v := range tt.dockerSecrets {
				if err := os.WriteFile(path.Join(secretsPath, k), []byte(v), 0o644); err != nil {
					t.Fatalf("failed to write Docker secret: %v", err)
				}
			}

			// Set up the environment
			for k, v := range tt.envVars {
				if err := os.Setenv(k, v); err != nil {
					t.Fatalf("failed to set environment variable: %v", err)
				}
			}

			// Run the test
			cfg, err := GetAppConfig(secretsPath)
			if !errors.Is(err, tt.expectedErr) {
				t.Errorf("expected error to be '%v', got '%v'", tt.expectedErr, err)
			}

			if tt.dockerSecrets != nil {
				// Compare the config values with the expected values
				if cfg.WebhookSecret != tt.dockerSecrets["WEBHOOK_SECRET"] {
					t.Errorf("expected WebhookSecret to be '%s', got '%s'", tt.dockerSecrets["WEBHOOK_SECRET"], cfg.WebhookSecret)
				}

				if cfg.GitAccessToken != tt.dockerSecrets["GIT_ACCESS_TOKEN"] {
					t.Errorf("expected GitAccessToken to be '%s', got '%s'", tt.dockerSecrets["GIT_ACCESS_TOKEN"], cfg.GitAccessToken)
				}

				httpPort, err := strconv.ParseUint(tt.envVars["HTTP_PORT"], 10, 16)
				if err != nil {
					t.Fatalf("failed to parse HTTP_PORT: %v", err)
				}

				if cfg.HttpPort != uint16(httpPort) {
					t.Errorf("expected HttpPort to be '%d', got '%d'", httpPort, cfg.HttpPort)
				}
			}
		})
	}
}
