package secretprovider_test

import (
	"errors"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/secretprovider/azurekeyvault"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
)

func TestInitialize(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	c, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	secretProvider, err := secretprovider.Initialize(ctx, c.SecretProvider, "v0.0.0-test")
	if err != nil {
		if errors.Is(err, bitwardensecretsmanager.ErrNotSupported) {
			t.Skip(err.Error())
		}

		return
	}

	if secretProvider != nil {
		t.Cleanup(func() {
			secretProvider.Close()
		})
	}
}

func TestInitializeAzureKeyVault(t *testing.T) {
	t.Setenv("SECRET_PROVIDER_VAULT_URL", "https://example.vault.azure.net")
	t.Setenv("AZURE_TOKEN_CREDENTIALS", "prod")
	t.Setenv("AZURE_AUTHORITY_HOST", "https://login.microsoftonline.com/")
	t.Setenv("AZURE_TENANT_ID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("AZURE_CLIENT_ID", "22222222-2222-2222-2222-222222222222")
	t.Setenv("AZURE_CLIENT_SECRET", "test-client-secret")

	provider, err := secretprovider.Initialize(t.Context(), azurekeyvault.Name, "v0.0.0-test")
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	t.Cleanup(provider.Close)

	if got := provider.Name(); got != azurekeyvault.Name {
		t.Fatalf("provider.Name() = %q, want %q", got, azurekeyvault.Name)
	}
}
