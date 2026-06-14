package app

import (
	"errors"
	"os"
	"path"
	"strconv"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

func TestGetConfig(t *testing.T) {
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
			expectedErr: config.ErrBothSecretsSet,
		},
		{
			name: "valid config with scoped git auth domains",
			envVars: map[string]string{
				"LOG_LEVEL":        "info",
				"HTTP_PORT":        "8080",
				"WEBHOOK_SECRET":   "secret",
				"GIT_AUTH_DOMAINS": "- domains:\n  - github.com\n  git_access_token: gh-token\n- domains:\n  - '*.example.com'\n  ssh_private_key: test-key\n  ssh_private_key_passphrase: pass",
			},
			dockerSecrets: nil,
			expectedErr:   nil,
		},
		{
			name: "valid config with scoped git auth domains from file",
			envVars: map[string]string{
				"LOG_LEVEL":      "info",
				"HTTP_PORT":      "8080",
				"WEBHOOK_SECRET": "secret",
			},
			dockerSecrets: map[string]string{
				"GIT_AUTH_DOMAINS": "- domains:\n  - gitlab.com\n  git_access_token: gl-token",
			},
			expectedErr: nil,
		},
		{
			name: "config with duplicate scoped git auth domains",
			envVars: map[string]string{
				"LOG_LEVEL":        "info",
				"HTTP_PORT":        "8080",
				"WEBHOOK_SECRET":   "secret",
				"GIT_AUTH_DOMAINS": "- domains:\n  - github.com\n  git_access_token: gh-token",
			},
			dockerSecrets: map[string]string{
				"GIT_AUTH_DOMAINS": "- domains:\n  - gitlab.com\n  git_access_token: gl-token",
			},
			expectedErr: config.ErrBothSecretsSet,
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
			cfg, err := GetConfig()
			if err != nil {
				if errors.Is(err, tt.expectedErr) {
					return
				}

				t.Fatalf("expected error to be '%v', got '%v'", tt.expectedErr, err)
			}

			if tt.dockerSecrets != nil {
				// Compare the config values with the expected values
				if expectedWebhookSecret, ok := tt.dockerSecrets["WEBHOOK_SECRET"]; ok && cfg.WebhookSecret != expectedWebhookSecret {
					t.Errorf("expected WebhookSecret to be '%s', got '%s'", expectedWebhookSecret, cfg.WebhookSecret)
				}

				if expectedGitAccessToken, ok := tt.dockerSecrets["GIT_ACCESS_TOKEN"]; ok && cfg.GitAccessToken != expectedGitAccessToken {
					t.Errorf("expected GitAccessToken to be '%s', got '%s'", expectedGitAccessToken, cfg.GitAccessToken)
				}

				httpPort, err := strconv.ParseUint(tt.envVars["HTTP_PORT"], 10, 16)
				if err != nil {
					t.Fatalf("failed to parse HTTP_PORT: %v", err)
				}

				if cfg.HttpPort != uint16(httpPort) {
					t.Errorf("expected HttpPort to be '%d', got '%d'", httpPort, cfg.HttpPort)
				}
			}

			if _, ok := tt.envVars["GIT_AUTH_DOMAINS"]; ok {
				if len(cfg.GitAuthDomains) != 2 {
					t.Fatalf("expected 2 scoped git auth entries, got %d", len(cfg.GitAuthDomains))
				}

				if cfg.GitAuthDomains[0].GitAccessToken != "gh-token" {
					t.Fatalf("expected first scoped token to be 'gh-token', got '%s'", cfg.GitAuthDomains[0].GitAccessToken)
				}

				if len(cfg.GitAuthDomains[1].Domains) != 1 || cfg.GitAuthDomains[1].Domains[0] != "*.example.com" {
					t.Fatalf("expected wildcard domain '*.example.com', got '%v'", cfg.GitAuthDomains[1].Domains)
				}
			}

			if tt.dockerSecrets != nil {
				if _, ok := tt.dockerSecrets["GIT_AUTH_DOMAINS"]; ok {
					if len(cfg.GitAuthDomains) != 1 {
						t.Fatalf("expected 1 scoped git auth entry from file, got %d", len(cfg.GitAuthDomains))
					}

					if cfg.GitAuthDomains[0].GitAccessToken != "gl-token" {
						t.Fatalf("expected scoped token from file to be 'gl-token', got '%s'", cfg.GitAuthDomains[0].GitAccessToken)
					}
				}
			}
		})
	}
}

func TestGetConfig_GlobalGitHubAppValidation(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("GIT_ACCESS_TOKEN", "")
	t.Setenv("GIT_ACCESS_TOKEN_FILE", "")
	t.Setenv("GITHUB_APP_ID", "12345")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "test-private-key")

	if _, err := GetConfig(); err != nil {
		t.Fatalf("expected global GitHub App config to be accepted, got %v", err)
	}
}

func TestGetConfig_GlobalGitHubAppRejectsTokenMix(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("GIT_ACCESS_TOKEN_FILE", "")
	t.Setenv("GITHUB_APP_ID", "12345")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "test-private-key")
	t.Setenv("GIT_ACCESS_TOKEN", "token")

	if _, err := GetConfig(); err == nil {
		t.Fatal("expected an error when combining GIT_ACCESS_TOKEN with global GitHub App credentials")
	}
}

func TestGetConfig_ScopedGitHubAppValidation(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("GIT_ACCESS_TOKEN", "")
	t.Setenv("GIT_ACCESS_TOKEN_FILE", "")
	t.Setenv("GIT_AUTH_DOMAINS", "- domains:\n  - github.com\n  github_app_id: '12345'\n  github_app_private_key: test-private-key")

	if _, err := GetConfig(); err != nil {
		t.Fatalf("expected scoped GitHub App config to be accepted, got %v", err)
	}
}

func TestGetConfig_ScopedGitHubAppRejectsTokenMix(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("GIT_ACCESS_TOKEN_FILE", "")
	t.Setenv("GIT_AUTH_DOMAINS", "- domains:\n  - github.com\n  git_access_token: gh-token\n  github_app_id: '12345'\n  github_app_private_key: test-private-key")

	if _, err := GetConfig(); err == nil {
		t.Fatal("expected an error when combining scoped git_access_token with scoped github app credentials")
	}
}

func TestGetConfig_OciTrustPolicyDefaultDisabled(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("OCI_TRUST_POLICY", "")
	t.Setenv("OCI_TRUST_POLICY_FILE", "")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	if cfg.OciTrustPolicy.Enabled {
		t.Fatal("expected OCI trust policy to be disabled by default")
	}
}

func TestGetConfig_OciTrustPolicyCanBeEnabled(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("OCI_TRUST_POLICY", "enabled: true")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	if !cfg.OciTrustPolicy.Enabled {
		t.Fatal("expected OCI trust policy to be enabled when configured")
	}
}

func TestGetConfig_OciVerifyMaxWorkersDefaultsToOne(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("OCI_VERIFY_MAX_WORKERS", "")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	if cfg.OciVerifyMaxWorkers != 1 {
		t.Fatalf("expected OCI_VERIFY_MAX_WORKERS default to be 1, got %d", cfg.OciVerifyMaxWorkers)
	}
}

func TestGetConfig_OciVerifyMaxWorkersRejectsZero(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("OCI_VERIFY_MAX_WORKERS", "0")

	if _, err := GetConfig(); err == nil {
		t.Fatal("expected OCI_VERIFY_MAX_WORKERS=0 to be rejected")
	}
}

func TestGetConfig_DataMountPathDefaultsToData(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("DATA_MOUNT_PATH", "")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	if cfg.DataMountPath != "/data" {
		t.Fatalf("expected DATA_MOUNT_PATH default to be %q, got %q", "/data", cfg.DataMountPath)
	}
}

func TestGetConfig_DataMountPathOverrideIsNormalized(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("DATA_MOUNT_PATH", " /opt/stacks/ ")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	if cfg.DataMountPath != "/opt/stacks" {
		t.Fatalf("expected DATA_MOUNT_PATH to normalize to %q, got %q", "/opt/stacks", cfg.DataMountPath)
	}
}

func TestGetConfig_DataMountPathRejectsRelativePath(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("DATA_MOUNT_PATH", "opt/stacks")

	if _, err := GetConfig(); err == nil {
		t.Fatal("expected relative DATA_MOUNT_PATH to be rejected")
	}
}
