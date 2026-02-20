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
					// Temporarily unset the original environment variable if it exists to avoid conflicts with the *_FILE variable
					if _, exists := os.LookupEnv(k); exists {
						t.Setenv(k, "")
					}

					secretFileEnvVar := k + "_FILE"
					secretFilePath := path.Join(secretsPath, k)

					// Set the app config *_FILE environment variable
					t.Logf("Set environment file variable %s to %s with content '%s'", secretFileEnvVar, secretFilePath, v)

					t.Setenv(secretFileEnvVar, secretFilePath)

					if err := os.WriteFile(secretFilePath, []byte(v), filesystem.PermOwner); err != nil {
						t.Fatalf("failed to write Docker secret: %v", err)
					}
				}
			}

			// Set up the environment
			for k, v := range tt.envVars {
				t.Logf("Set environment variable %s to %s", k, v)
				t.Setenv(k, v)
			}

			// Run the test
			cfg, err := GetAppConfig()
			if err != nil {
				if errors.Is(err, tt.expectedErr) {
					return
				}

				t.Fatalf("expected error to be '%v', got '%v'", tt.expectedErr, err)
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
