package azurekeyvault

import (
	"errors"
	"os"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
)

func TestGetConfig(t *testing.T) {
	t.Run("valid vault URL", func(t *testing.T) {
		t.Setenv("SECRET_PROVIDER_VAULT_URL", "https://example.vault.azure.net")

		cfg, err := GetConfig()
		if err != nil {
			t.Fatalf("GetConfig() error = %v", err)
		}

		if got, want := string(cfg.VaultURL), "https://example.vault.azure.net"; got != want {
			t.Fatalf("VaultURL = %q, want %q", got, want)
		}
	})

	for _, test := range []struct {
		name     string
		vaultURL string
	}{
		{name: "missing vault URL"},
		{name: "invalid vault URL", vaultURL: "example.vault.azure.net"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("SECRET_PROVIDER_VAULT_URL", test.vaultURL)

			_, err := GetConfig()
			if err == nil {
				t.Fatal("GetConfig() error = nil, want error")
			}

			if !errors.Is(err, config.ErrParseConfigFailed) {
				t.Fatalf("GetConfig() error = %v, want ErrParseConfigFailed", err)
			}
		})
	}
}

func TestGetConfigClientSecretFile(t *testing.T) {
	secretFile := writeClientSecretFile(t, "client-secret\n")

	t.Setenv("SECRET_PROVIDER_VAULT_URL", "https://example.vault.azure.net")
	t.Setenv("AZURE_CLIENT_SECRET", "")
	t.Setenv("AZURE_CLIENT_SECRET_FILE", secretFile)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}

	if got, want := cfg.ClientSecret, "client-secret"; got != want {
		t.Fatalf("ClientSecret = %q, want %q", got, want)
	}

	if cfg.ClientSecretFile == "" {
		t.Fatal("ClientSecretFile is empty, want file configuration marker")
	}
}

func TestGetConfigRejectsClientSecretAndFile(t *testing.T) {
	secretFile := writeClientSecretFile(t, "file-client-secret")

	t.Setenv("SECRET_PROVIDER_VAULT_URL", "https://example.vault.azure.net")
	t.Setenv("AZURE_CLIENT_SECRET", "environment-client-secret")
	t.Setenv("AZURE_CLIENT_SECRET_FILE", secretFile)

	_, err := GetConfig()
	if !errors.Is(err, config.ErrParseConfigFailed) {
		t.Fatalf("GetConfig() error = %v, want ErrParseConfigFailed", err)
	}
}

func TestGetConfigRejectsEmptyClientSecretFile(t *testing.T) {
	secretFile := writeClientSecretFile(t, "")

	t.Setenv("SECRET_PROVIDER_VAULT_URL", "https://example.vault.azure.net")
	t.Setenv("AZURE_CLIENT_SECRET", "")
	t.Setenv("AZURE_CLIENT_SECRET_FILE", secretFile)

	_, err := GetConfig()
	if !errors.Is(err, config.ErrParseConfigFailed) {
		t.Fatalf("GetConfig() error = %v, want ErrParseConfigFailed", err)
	}
}

func writeClientSecretFile(t *testing.T, value string) string {
	t.Helper()

	path := t.TempDir() + "/azure-client-secret"
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("write client secret file: %v", err)
	}

	return path
}
