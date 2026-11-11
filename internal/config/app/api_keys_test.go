package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/restapi"
)

func isolateAPIKeyEnv(t *testing.T) {
	t.Helper()

	t.Setenv("API_KEYS", "")
	t.Setenv("API_KEYS_FILE", "")
	t.Setenv("MCP_ENABLED", "false")
	t.Setenv("WEBHOOK_SECRET", "test-webhook-secret")
	setGitAuthEnv(t, "", "")

	for _, name := range []string{"API_SECRET", "API_SECRET_FILE"} {
		t.Setenv(name, "")

		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

func TestGetConfigAPIKeys(t *testing.T) {
	const secret = "sensitive-test-key"

	for _, testCase := range []struct {
		name    string
		input   string
		want    restapi.Keyring
		wantErr string
	}{
		{name: "unset"},
		{name: "empty list", input: "[]"},
		{name: "blank document", input: " \n", wantErr: "YAML list"},
		{
			name:  "multiple roles",
			input: "- key: sensitive-test-key\n  role: read\n- key: inspector\n  role: inspect\n- key: writer\n  role: write\n- key: admin\n  role: full",
			want: restapi.Keyring{
				{Key: secret, Role: restapi.RoleRead},
				{Key: "inspector", Role: restapi.RoleInspect},
				{Key: "writer", Role: restapi.RoleWrite},
				{Key: "admin", Role: restapi.RoleFull},
			},
		},
		{name: "malformed YAML", input: "- key: sensitive-test-key\n  role: [", wantErr: "YAML list"},
		{name: "mapping instead of list", input: "key: sensitive-test-key\nrole: read", wantErr: "YAML list"},
		{name: "null instead of list", input: "null", wantErr: "YAML list"},
		{name: "unknown field", input: "- key: sensitive-test-key\n  role: read\n  typo: true", wantErr: "YAML list"},
		{name: "duplicate field", input: "- key: sensitive-test-key\n  key: other\n  role: read", wantErr: "YAML list"},
		{name: "missing key", input: "- role: read", wantErr: "key must not be empty"},
		{name: "empty key", input: "- key: '  '\n  role: read", wantErr: "key must not be empty"},
		{name: "leading whitespace", input: "- key: ' sensitive-test-key'\n  role: read", wantErr: "valid HTTP header"},
		{name: "invalid header character", input: "- key: \"sensitive-test-key\\nother\"\n  role: read", wantErr: "valid HTTP header"},
		{name: "missing role", input: "- key: sensitive-test-key", wantErr: "role must be"},
		{name: "invalid role", input: "- key: sensitive-test-key\n  role: admin", wantErr: "role must be"},
		{
			name:    "duplicate key",
			input:   "- key: sensitive-test-key\n  role: read\n- key: sensitive-test-key\n  role: full",
			wantErr: "duplicate key",
		},
		{name: "multiple documents", input: "- key: sensitive-test-key\n  role: read\n---\n[]", wantErr: "exactly one"},
		{name: "invalid second document", input: "- key: sensitive-test-key\n  role: read\n---\n[", wantErr: "exactly one"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			isolateAPIKeyEnv(t)
			t.Setenv("API_KEYS", testCase.input)

			cfg, err := GetConfig()
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("GetConfig() error = %v, want %q", err, testCase.wantErr)
				}

				if strings.Contains(err.Error(), secret) {
					t.Fatal("GetConfig() exposed a key in its error")
				}

				return
			}

			if err != nil {
				t.Fatalf("GetConfig() error = %v", err)
			}

			if len(cfg.APIKeys) != len(testCase.want) {
				t.Fatalf("got %d API keys, want %d", len(cfg.APIKeys), len(testCase.want))
			}

			for i, want := range testCase.want {
				if cfg.APIKeys[i] != want {
					t.Fatalf("API key entry %d did not match", i+1)
				}
			}
		})
	}
}

func TestGetConfigLegacyAPIKeys(t *testing.T) {
	for _, name := range []string{"API_SECRET", "API_SECRET_FILE"} {
		for _, value := range []string{"", "old-sensitive-value"} {
			t.Run(name+"/"+value, func(t *testing.T) {
				isolateAPIKeyEnv(t)
				t.Setenv("API_KEYS", "- key: new-key\n  role: full")
				t.Setenv(name, value)

				_, err := GetConfig()
				if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "API_KEYS") {
					t.Fatalf("GetConfig() error = %v, want migration guidance for %s", err, name)
				}

				if strings.Contains(err.Error(), "old-sensitive-value") {
					t.Fatal("GetConfig() exposed the legacy value")
				}
			})
		}
	}
}

func TestGetConfigAPIKeysFile(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		content   string
		envValue  string
		missing   bool
		wantErr   string
		wantCount int
	}{
		{name: "valid file", content: "- key: from-file\n  role: write", wantCount: 1},
		{name: "empty list", content: "[]"},
		{name: "empty file", wantErr: "must contain a YAML list"},
		{name: "invalid file", content: "- key: sensitive-test-key\n  role: [", wantErr: "YAML list"},
		{name: "missing file", missing: true, wantErr: "APIKeysFile"},
		{name: "both settings", content: "[]", envValue: "[]", wantErr: "API_KEYS or API_KEYS_FILE"},
		{name: "both settings with empty file", envValue: "[]", wantErr: "API_KEYS or API_KEYS_FILE"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			isolateAPIKeyEnv(t)
			filename := filepath.Join(t.TempDir(), "keys")

			if !testCase.missing {
				if err := os.WriteFile(filename, []byte(testCase.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			t.Setenv("API_KEYS", testCase.envValue)
			t.Setenv("API_KEYS_FILE", filename)

			cfg, err := GetConfig()
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("GetConfig() error = %v, want %q", err, testCase.wantErr)
				}

				if strings.Contains(err.Error(), "sensitive-test-key") {
					t.Fatal("GetConfig() exposed a key in its error")
				}

				return
			}

			if err != nil {
				t.Fatalf("GetConfig() error = %v", err)
			}

			if len(cfg.APIKeys) != testCase.wantCount {
				t.Fatalf("got %d API keys, want %d", len(cfg.APIKeys), testCase.wantCount)
			}
		})
	}
}

func TestAPIKeysAreOmittedFromSerializedConfig(t *testing.T) {
	t.Parallel()

	const secret = "sensitive-test-key"

	cfg := Config{
		APIKeysYAML: secret,
		APIKeysFile: secret,
		APIKeys:     restapi.Keyring{{Key: secret, Role: restapi.RoleRead}},
	}

	for _, format := range []struct {
		name    string
		marshal func(any) ([]byte, error)
	}{
		{"JSON", json.Marshal},
		{"YAML", yaml.Marshal},
	} {
		t.Run(format.name, func(t *testing.T) {
			t.Parallel()

			data, err := format.marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}

			if strings.Contains(string(data), secret) {
				t.Fatal("serialized config contains an API key")
			}
		})
	}
}

func TestGetConfigEncryptedAPIKeysFile(t *testing.T) {
	isolateAPIKeyEnv(t)
	t.Setenv("SOPS_AGE_KEY", "")
	encryption.SetupAgeKeyEnvVar(t)
	t.Setenv("API_KEYS_FILE", filepath.Join("testdata", "api-keys.enc"))

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}

	want := restapi.Keyring{
		{Key: "fixture-reader", Role: restapi.RoleRead},
		{Key: "fixture-writer", Role: restapi.RoleWrite},
	}

	if len(cfg.APIKeys) != len(want) {
		t.Fatalf("got %d API keys, want %d", len(cfg.APIKeys), len(want))
	}

	for i := range want {
		if cfg.APIKeys[i] != want[i] {
			t.Fatalf("API key entry %d did not match", i+1)
		}
	}
}
