package bitwardenvault

import (
	"context"
	"testing"
)

// Integration test for Provider against a local Vaultwarden instance.
// Requires a running Vaultwarden container (see testdata/docker-compose.yml)
// and a test vault with known item IDs and API key.
func TestProvider_GetSecret(t *testing.T) {
	// Get config
	cfg, cfgErr := GetConfig()
	if cfgErr != nil {
		t.Fatalf("failed to get config: %v", cfgErr)
	}

	testSecretID := "13591cbd-4f0d-4fac-8522-020594387b28" // #nosec G101

	provider, err := NewProvider(cfg.ApiUrl, cfg.OAuth2TokenURL, cfg.OAuth2ClientID, cfg.OAuth2ClientSecret)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	t.Run("login secret", func(t *testing.T) {
		val, err := provider.GetSecret(context.Background(), testSecretID)
		if err != nil {
			t.Logf("full error: %v", err)
			t.Fatalf("failed to get login secret: %v", err)
		}

		if val == "" {
			t.Error("expected non-empty login password")
		}
	})

	t.Run("ssh key secret", func(t *testing.T) {
		val, err := provider.GetSecret(context.Background(), testSecretID)
		if err != nil {
			t.Logf("full error: %v", err)
			t.Fatalf("failed to get ssh key secret: %v", err)
		}

		if val == "" {
			t.Error("expected non-empty ssh private key")
		}
	})
}
