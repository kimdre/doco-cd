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

	testSecretID := "7293a970-2834-4490-a2c9-b41400c9295f" // #nosec G101

	provider, err := NewProvider(t.Context(), cfg)
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
