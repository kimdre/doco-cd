package app

import (
	"errors"
	"net/netip"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

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
				"GIT_ACCESS_TOKEN_USER": "oauth2",
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
				"GIT_ACCESS_TOKEN_USER": "oauth2",
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
				"GIT_ACCESS_TOKEN_USER": "oauth2",
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

func TestGetConfig_SchedulerEnabled(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("SCHEDULER_ENABLED", "false")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected scheduler config to be accepted, got %v", err)
	}

	if cfg.SchedulerEnabled {
		t.Fatal("expected SchedulerEnabled to be false")
	}
}

func TestGetConfig_McpEnabled(t *testing.T) {
	tests := []struct {
		name        string
		mcpEnabled  string
		apiSecret   string
		wantEnabled bool
		wantErr     string
	}{
		{
			name: "disabled by default",
		},
		{
			name:        "enabled with API secret",
			mcpEnabled:  "true",
			apiSecret:   "x",
			wantEnabled: true,
		},
		{
			name:       "enabled without API secret",
			mcpEnabled: "true",
			wantErr:    "MCP_ENABLED requires API_SECRET",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", "info")
			t.Setenv("HTTP_PORT", "8080")
			t.Setenv("WEBHOOK_SECRET", "secret")
			t.Setenv("MCP_ENABLED", testCase.mcpEnabled)
			t.Setenv("API_SECRET", testCase.apiSecret)
			t.Setenv("API_SECRET_FILE", "")

			cfg, err := GetConfig()
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("expected error containing %q, got %v", testCase.wantErr, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("expected config to load, got %v", err)
			}

			if cfg.McpEnabled != testCase.wantEnabled {
				t.Fatalf("expected McpEnabled=%v, got %v", testCase.wantEnabled, cfg.McpEnabled)
			}
		})
	}

	t.Run("enabled with API secret file", func(t *testing.T) {
		const apiSecret = "file-secret"

		apiSecretFile := path.Join(t.TempDir(), "api-secret")
		if err := os.WriteFile(apiSecretFile, []byte(apiSecret), filesystem.PermOwner); err != nil {
			t.Fatalf("failed to write API secret file: %v", err)
		}

		t.Setenv("LOG_LEVEL", "info")
		t.Setenv("HTTP_PORT", "8080")
		t.Setenv("WEBHOOK_SECRET", "secret")
		t.Setenv("MCP_ENABLED", "true")
		t.Setenv("API_SECRET", "")
		t.Setenv("API_SECRET_FILE", apiSecretFile)

		cfg, err := GetConfig()
		if err != nil {
			t.Fatalf("expected config to load, got %v", err)
		}

		if !cfg.McpEnabled {
			t.Fatal("expected McpEnabled to be true")
		}

		if cfg.ApiSecret != apiSecret {
			t.Fatalf("expected ApiSecret=%q, got %q", apiSecret, cfg.ApiSecret)
		}
	})
}

func TestGetConfig_OpenAPIEnabled(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		value       string
		wantEnabled bool
	}{
		{
			name: "disabled by default",
		},
		{
			name:        "enabled",
			value:       "true",
			wantEnabled: true,
		},
		{
			name:  "explicitly disabled",
			value: "false",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("OPENAPI_ENABLED", testCase.value)
			t.Setenv("WEBHOOK_SECRET", "secret")
			t.Setenv("API_SECRET", "")
			t.Setenv("API_SECRET_FILE", "")

			cfg, err := GetConfig()
			if err != nil {
				t.Fatalf("load config: %v", err)
			}

			if cfg.OpenAPIEnabled != testCase.wantEnabled {
				t.Fatalf("OpenAPIEnabled = %v, want %v", cfg.OpenAPIEnabled, testCase.wantEnabled)
			}
		})
	}
}

func TestGetConfig_Pprof(t *testing.T) {
	tests := []struct {
		name        string
		enabled     string
		port        string
		metricsPort string
		wantEnabled bool
		wantPort    uint16
		wantErr     string
	}{
		{
			name:     "disabled by default",
			wantPort: 6060,
		},
		{
			name:        "enabled with custom port",
			enabled:     "true",
			port:        "6061",
			wantEnabled: true,
			wantPort:    6061,
		},
		{
			name:    "rejects HTTP port conflict",
			enabled: "true",
			port:    "8080",
			wantErr: "PPROF_PORT and HTTP_PORT cannot be the same",
		},
		{
			name:        "rejects metrics port conflict",
			enabled:     "true",
			port:        "6061",
			metricsPort: "6061",
			wantErr:     "PPROF_PORT and METRICS_PORT cannot be the same",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", "info")
			t.Setenv("HTTP_PORT", "8080")
			t.Setenv("WEBHOOK_SECRET", "secret")
			t.Setenv("PPROF_ENABLED", testCase.enabled)
			t.Setenv("PPROF_PORT", testCase.port)
			t.Setenv("METRICS_PORT", testCase.metricsPort)

			cfg, err := GetConfig()
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("expected error containing %q, got %v", testCase.wantErr, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("expected config to load, got %v", err)
			}

			if cfg.PprofEnabled != testCase.wantEnabled {
				t.Fatalf("PprofEnabled = %t, want %t", cfg.PprofEnabled, testCase.wantEnabled)
			}

			if cfg.PprofPort != testCase.wantPort {
				t.Fatalf("PprofPort = %d, want %d", cfg.PprofPort, testCase.wantPort)
			}
		})
	}
}

func TestGetConfigRejectsNonPositiveMaxPayloadSize(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("MAX_PAYLOAD_SIZE", value)
			t.Setenv("WEBHOOK_SECRET", "secret")
			t.Setenv("API_SECRET", "")
			t.Setenv("API_SECRET_FILE", "")

			if _, err := GetConfig(); err == nil {
				t.Fatalf("expected MAX_PAYLOAD_SIZE=%s to be rejected", value)
			}
		})
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

func TestGetConfig_OciInsecureRegistries(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("OCI_INSECURE_REGISTRIES", " registry.example:5000,REGISTRY.example:5000,localhost ")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	if got, want := strings.Join(cfg.OciInsecureRegistries, ","), "registry.example:5000,localhost"; got != want {
		t.Fatalf("expected OCI insecure registries %q, got %q", want, got)
	}
}

func TestGetConfig_TrustedProxyNetworks(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("TRUSTED_PROXY_NETWORKS", " 127.0.0.1/8, 10.0.0.0/8,10.0.0.0/8, ::1/128 ")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	got := make([]string, 0, len(cfg.TrustedProxyNetworks))
	for _, network := range cfg.TrustedProxyNetworks {
		got = append(got, network.String())
	}

	want := []string{netip.MustParsePrefix("127.0.0.0/8").String(), "10.0.0.0/8", "::1/128"}
	if len(got) != len(want) {
		t.Fatalf("expected %d trusted proxy networks, got %d (%v)", len(want), len(got), got)
	}

	for i, expected := range want {
		if got[i] != expected {
			t.Fatalf("expected trusted proxy network %d to be %q, got %q", i, expected, got[i])
		}
	}
}

func TestGetConfig_TrustedProxyHeader(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	if cfg.TrustedProxyHeader != "X-Forwarded-For" {
		t.Fatalf("expected trusted proxy header to default to X-Forwarded-For, got %q", cfg.TrustedProxyHeader)
	}
}

func TestGetConfig_TrustedProxyHeaderOverride(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("TRUSTED_PROXY_HEADER", "X-Client-IP")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	if cfg.TrustedProxyHeader != "X-Client-IP" {
		t.Fatalf("expected trusted proxy header to be set, got %q", cfg.TrustedProxyHeader)
	}
}

func TestGetConfig_HTTPTLS(t *testing.T) {
	testCases := []struct {
		name         string
		certFile     string
		keyFile      string
		wantErr      bool
		wantEnabled  bool
		wantCertFile string
		wantKeyFile  string
	}{
		{
			name:        "defaults disabled",
			wantEnabled: false,
		},
		{
			name:         "enabled with cert and key",
			certFile:     " /tls/server.crt ",
			keyFile:      " /tls/server.key ",
			wantEnabled:  true,
			wantCertFile: "/tls/server.crt",
			wantKeyFile:  "/tls/server.key",
		},
		{
			name:     "cert without key",
			certFile: "/tls/server.crt",
			wantErr:  true,
		},
		{
			name:    "key without cert",
			keyFile: "/tls/server.key",
			wantErr: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", "info")
			t.Setenv("HTTP_PORT", "8080")
			t.Setenv("WEBHOOK_SECRET", "secret")

			if testCase.certFile != "" {
				t.Setenv("HTTP_TLS_CERT_FILE", testCase.certFile)
			}

			if testCase.keyFile != "" {
				t.Setenv("HTTP_TLS_KEY_FILE", testCase.keyFile)
			}

			cfg, err := GetConfig()
			if testCase.wantErr {
				if err == nil {
					t.Fatal("expected TLS config validation error")
				}

				return
			}

			if err != nil {
				t.Fatalf("expected config to load, got %v", err)
			}

			if cfg.HttpTLSEnabled != testCase.wantEnabled {
				t.Fatalf("expected HttpTLSEnabled=%v, got %v", testCase.wantEnabled, cfg.HttpTLSEnabled)
			}

			if cfg.HttpTLSCertFile != testCase.wantCertFile {
				t.Fatalf("expected HttpTLSCertFile=%q, got %q", testCase.wantCertFile, cfg.HttpTLSCertFile)
			}

			if cfg.HttpTLSKeyFile != testCase.wantKeyFile {
				t.Fatalf("expected HttpTLSKeyFile=%q, got %q", testCase.wantKeyFile, cfg.HttpTLSKeyFile)
			}
		})
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

func TestGetConfig_DataHostPath(t *testing.T) {
	testCases := []struct {
		name         string
		value        string
		expected     string
		unsetEnv     bool
		expectsError bool
	}{
		{name: "defaults to empty", unsetEnv: true},
		{name: "treats explicit empty as empty"},
		{name: "treats whitespace as empty", value: "   "},
		{name: "normalizes override", value: " /srv/doco-cd/../data/ ", expected: "/srv/data"},
		{name: "rejects relative path", value: "srv/doco-cd", expectsError: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", "info")
			t.Setenv("HTTP_PORT", "8080")
			t.Setenv("WEBHOOK_SECRET", "secret")

			if testCase.unsetEnv {
				originalValue, wasSet := os.LookupEnv("DATA_HOST_PATH")

				if err := os.Unsetenv("DATA_HOST_PATH"); err != nil {
					t.Fatalf("failed to unset DATA_HOST_PATH: %v", err)
				}

				t.Cleanup(func() {
					if wasSet {
						if err := os.Setenv("DATA_HOST_PATH", originalValue); err != nil {
							t.Errorf("failed to restore DATA_HOST_PATH: %v", err)
						}

						return
					}

					if err := os.Unsetenv("DATA_HOST_PATH"); err != nil {
						t.Errorf("failed to keep DATA_HOST_PATH unset: %v", err)
					}
				})
			} else {
				t.Setenv("DATA_HOST_PATH", testCase.value)
			}

			cfg, err := GetConfig()
			if testCase.expectsError {
				if err == nil || !strings.Contains(err.Error(), "DATA_HOST_PATH") || !strings.Contains(err.Error(), testCase.value) {
					t.Fatalf("expected DATA_HOST_PATH %q validation error, got %v", testCase.value, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("expected config to load, got %v", err)
			}

			if cfg.DataHostPath != testCase.expected {
				t.Fatalf("expected DATA_HOST_PATH %q, got %q", testCase.expected, cfg.DataHostPath)
			}
		})
	}
}

func TestGetConfig_GitScmApiUrlValidation(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("GIT_SCM_API_URL", "https://git.example.com")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	if string(cfg.GitScmApiUrl) != "https://git.example.com" {
		t.Fatalf("expected GIT_SCM_API_URL to be set, got %q", cfg.GitScmApiUrl)
	}
}

func TestGetConfig_GitScmApiUrlRejectsNonHTTP(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("GIT_SCM_API_URL", "ssh://git.example.com:2222")

	if _, err := GetConfig(); err == nil {
		t.Fatal("expected non-http GIT_SCM_API_URL to be rejected")
	}
}

func TestGetConfig_SourceURLRewrites(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("SOURCE_URL_REWRITES", "HTTPS://Forgejo.Example.Com/: http://forgejo:3000/")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	overrideURL, ok := cfg.SourceURLRewrites["https://forgejo.example.com/"]
	if !ok {
		t.Fatalf("expected normalized override key %q to exist", "https://forgejo.example.com/")
	}

	if overrideURL != "http://forgejo:3000/" {
		t.Fatalf("expected override URL to be %q, got %q", "http://forgejo:3000/", overrideURL)
	}
}

func TestGetConfig_SourceURLRewritesFromFile(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("SOURCE_URL_REWRITES", "")

	overridesFile := path.Join(t.TempDir(), "webhook-repository-overrides.yaml")
	overridesYAML := "forgejo.example.com: forgejo:3000\n"

	if err := os.WriteFile(overridesFile, []byte(overridesYAML), filesystem.PermOwner); err != nil {
		t.Fatalf("failed to write overrides file: %v", err)
	}

	t.Setenv("SOURCE_URL_REWRITES_FILE", overridesFile)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	overrideURL, ok := cfg.SourceURLRewrites["forgejo.example.com"]
	if !ok || overrideURL != "forgejo:3000" {
		t.Fatalf("expected file-based rewrite to be loaded, got %+v", cfg.SourceURLRewrites)
	}
}

func TestGetConfig_SourceURLRewritesRejectsEmptyTarget(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("SOURCE_URL_REWRITES", "forgejo.example.com: ''")

	if _, err := GetConfig(); err == nil {
		t.Fatal("expected empty source URL rewrite target to be rejected")
	}
}

func TestGetConfig_SourceURLRewritesRejectsEnvAndFileTogether(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("SOURCE_URL_REWRITES", "forgejo.example.com: forgejo:3000")

	overridesFile := path.Join(t.TempDir(), "webhook-repository-overrides.yaml")
	if err := os.WriteFile(overridesFile, []byte("https://forgejo.example.com/: http://forgejo:3000/\n"), filesystem.PermOwner); err != nil {
		t.Fatalf("failed to write overrides file: %v", err)
	}

	t.Setenv("SOURCE_URL_REWRITES_FILE", overridesFile)

	if _, err := GetConfig(); err == nil {
		t.Fatal("expected config error when both SOURCE_URL_REWRITES and _FILE are set")
	}
}

func TestGetConfig_DockerSwarmConfigRetentionDefaultsToZero(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("DOCKER_SWARM_CONFIG_RETENTION", "")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	if cfg.DockerSwarmConfigRetention != 0 {
		t.Fatalf("expected DOCKER_SWARM_CONFIG_RETENTION default to be 0, got %d", cfg.DockerSwarmConfigRetention)
	}
}

func TestGetConfig_DockerSwarmConfigRetentionAllowsMinusOne(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("DOCKER_SWARM_CONFIG_RETENTION", "-1")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected DOCKER_SWARM_CONFIG_RETENTION=-1 to be accepted, got %v", err)
	}

	if cfg.DockerSwarmConfigRetention != -1 {
		t.Fatalf("expected DOCKER_SWARM_CONFIG_RETENTION=-1, got %d", cfg.DockerSwarmConfigRetention)
	}
}

func TestGetConfig_DockerSwarmConfigRetentionRejectsBelowMinusOne(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("DOCKER_SWARM_CONFIG_RETENTION", "-2")

	if _, err := GetConfig(); err == nil {
		t.Fatal("expected DOCKER_SWARM_CONFIG_RETENTION=-2 to be rejected")
	}
}

func TestGetConfig_DockerSwarmSecretRetentionDefaultsToZero(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("DOCKER_SWARM_SECRET_RETENTION", "")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected config to load, got %v", err)
	}

	if cfg.DockerSwarmSecretRetention != 0 {
		t.Fatalf("expected DOCKER_SWARM_SECRET_RETENTION default to be 0, got %d", cfg.DockerSwarmSecretRetention)
	}
}

func TestGetConfig_DockerSwarmSecretRetentionAllowsMinusOne(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("DOCKER_SWARM_SECRET_RETENTION", "-1")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected DOCKER_SWARM_SECRET_RETENTION=-1 to be accepted, got %v", err)
	}

	if cfg.DockerSwarmSecretRetention != -1 {
		t.Fatalf("expected DOCKER_SWARM_SECRET_RETENTION=-1, got %d", cfg.DockerSwarmSecretRetention)
	}
}

func TestGetConfig_DockerSwarmSecretRetentionRejectsBelowMinusOne(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("DOCKER_SWARM_SECRET_RETENTION", "-2")

	if _, err := GetConfig(); err == nil {
		t.Fatal("expected DOCKER_SWARM_SECRET_RETENTION=-2 to be rejected")
	}
}

func TestGetConfig_PollConfigAbsolutePathNormalizedInPlace(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("POLL_CONFIG", `
- source: git
  url: /local-repos/my-app
  reference: refs/heads/main
`)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected poll config to be accepted, got %v", err)
	}

	if len(cfg.PollConfig) != 1 {
		t.Fatalf("expected 1 poll config, got %d", len(cfg.PollConfig))
	}

	want := "file:///local-repos/my-app"
	if got := cfg.PollConfig[0].SourceUrl; got != want {
		t.Fatalf("expected normalized SourceUrl %q to be persisted, got %q", want, got)
	}
}

func TestGetConfig_PollConfigExplicitZeroDisablesPolling(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("POLL_CONFIG", `
- source: oci
  url: ghcr.io/kimdre/doco-cd_tests:test
  interval: 0
- source: git
  url: https://github.com/kimdre/doco-cd_tests.git
  reference: main
  interval: 10
`)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected poll config to be accepted, got %v", err)
	}

	if len(cfg.PollConfig) != 2 {
		t.Fatalf("expected 2 poll configs, got %d", len(cfg.PollConfig))
	}

	if got := cfg.PollConfig[0].Interval; got != 0 {
		t.Fatalf("expected explicit zero interval to be preserved, got %s", got)
	}

	if got := cfg.PollConfig[1].Interval; got != 10*time.Second {
		t.Fatalf("expected Git interval to be 10s, got %s", got)
	}
}
