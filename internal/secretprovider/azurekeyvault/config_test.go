package azurekeyvault

import (
	"errors"
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
