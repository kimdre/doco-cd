package bitwardenvault

import (
	"context"
	"os"
	"testing"
)

// Integration test for Provider against a local Vaultwarden instance.
// Requires a running Vaultwarden container (see testdata/docker-compose.yml)
// and a test vault with known item IDs and API key.
func TestProvider_GetSecret(t *testing.T) {
	apiUrl := os.Getenv("VAULTWARDEN_API_URL")
	apiKey := os.Getenv("VAULTWARDEN_API_KEY")
	loginID := os.Getenv("VAULTWARDEN_TEST_LOGIN_ID")
	sshKeyID := os.Getenv("VAULTWARDEN_TEST_SSHKEY_ID")

	if apiUrl == "" || apiKey == "" || loginID == "" || sshKeyID == "" {
		t.Skip("Set VAULTWARDEN_API_URL, VAULTWARDEN_API_KEY, VAULTWARDEN_TEST_LOGIN_ID, VAULTWARDEN_TEST_SSHKEY_ID to run integration test")
	}

	provider := NewProvider(apiUrl, apiKey)

	t.Run("login secret", func(t *testing.T) {
		val, err := provider.GetSecret(context.Background(), loginID)
		if err != nil {
			t.Fatalf("failed to get login secret: %v", err)
		}

		if val == "" {
			t.Error("expected non-empty login password")
		}
	})

	t.Run("ssh key secret", func(t *testing.T) {
		val, err := provider.GetSecret(context.Background(), sshKeyID)
		if err != nil {
			t.Fatalf("failed to get ssh key secret: %v", err)
		}

		if val == "" {
			t.Error("expected non-empty ssh private key")
		}
	})
}
