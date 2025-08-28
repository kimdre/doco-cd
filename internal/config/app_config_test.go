package config

import (
	"errors"
	"os"
	"path"
	"strconv"
	"testing"

	"github.com/kimdre/doco-cd/internal/filesystem"
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
				"LOG_LEVEL":        "invalid",
				"WEBHOOK_SECRET":   "secret",
				"GIT_ACCESS_TOKEN": "token",
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
				"WEBHOOK_SECRET":   "webh00k_secret",
				"GIT_ACCESS_TOKEN": "t0ken",
			},
			expectedErr: nil,
		},
		{
			name: "config with duplicate secrets",
			envVars: map[string]string{
				"LOG_LEVEL":             "info",
				"HTTP_PORT":             "8080",
				"AUTH_TYPE":             "oauth2",
				"SKIP_TLS_VERIFICATION": "false",
				"WEBHOOK_SECRET":        "webh00k_secret",
			},
			dockerSecrets: map[string]string{
				"WEBHOOK_SECRET":   "webh00k_secret",
				"GIT_ACCESS_TOKEN": "t0ken",
			},
			expectedErr: ErrBothSecretsSet,
		},
	}

	// Restore environment variables after the test
	for _, k := range []string{"LOG_LEVEL", "HTTP_PORT", "WEBHOOK_SECRET", "GIT_ACCESS_TOKEN", "AUTH_TYPE", "SKIP_TLS_VERIFICATION"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() {
				err := os.Setenv(k, v)
				if err != nil {
					t.Fatalf("failed to restore environment variable %s: %v", k, err)
				}
			})
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.dockerSecrets != nil {
				secretsPath := path.Join(t.TempDir(), "/run/secrets/")

				// Create the Docker secrets directory
				if err := os.MkdirAll(secretsPath, filesystem.PermDir); err != nil {
					t.Fatalf("failed to create Docker secrets directory: %v", err)
				}

				// Set up Docker secrets as environment variables
				for k, v := range tt.dockerSecrets {
					secretFileEnvVar := k + "_FILE"
					secretFilePath := path.Join(secretsPath, k)

					t.Cleanup(func() {
						err := os.Unsetenv(secretFileEnvVar)
						if err != nil {
							return
						}
					})

					// Set the app config *_FILE environment variable
					t.Logf("Set environment variable %s to %s", secretFileEnvVar, secretFilePath)

					err := os.Setenv(secretFileEnvVar, secretFilePath)
					if err != nil {
						t.Fatalf("failed to set environment variable: %v", err)
					}

					t.Logf("Set Docker secret %s to %s", k, v)

					if err = os.WriteFile(secretFilePath, []byte(v), filesystem.PermOwner); err != nil {
						t.Fatalf("failed to write Docker secret: %v", err)
					}
				}
			}

			t.Cleanup(func() {
				// Clean up the environment
				for k := range tt.envVars {
					if err := os.Unsetenv(k); err != nil {
						t.Fatalf("failed to unset environment variable: %v", err)
					}
				}
			})

			// Set up the environment
			for k, v := range tt.envVars {
				if err := os.Setenv(k, v); err != nil {
					t.Fatalf("failed to set environment variable: %v", err)
				}
			}

			// Run the test
			cfg, err := GetAppConfig()
			if !errors.Is(err, tt.expectedErr) {
				t.Fatalf("expected error to be '%v', got '%v'", tt.expectedErr, err)
			}

			if tt.expectedErr != nil {
				return
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
