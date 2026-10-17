package secretprovider_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/secretprovider/azurekeyvault"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
	"github.com/kimdre/doco-cd/internal/secretprovider/openbao"
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

func TestInitializeOpenBaoTokenFile(t *testing.T) {
	var token atomic.Value
	token.Store("token-A")

	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)

		if r.Header.Get("X-Vault-Token") != token.Load().(string) {
			t.Error("request used an outdated token")
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)

			return
		}

		w.Header().Set("Content-Type", "application/json")

		if _, err := fmt.Fprint(w, `{"data":{"data":{"value":"resolved"}}}`); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("token-A"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SECRET_PROVIDER_SITE_URL", server.URL)
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "")
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", path)

	provider, err := secretprovider.Initialize(t.Context(), openbao.Name, "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(provider.Close)

	for _, next := range []string{"token-A", "token-B"} {
		token.Store(next)

		if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
			t.Fatal(err)
		}

		got, err := provider.GetSecret(t.Context(), "kv:secret:app:value")
		if err != nil || got != "resolved" {
			t.Fatalf("GetSecret() = %q, %v", got, err)
		}
	}

	if requests.Load() != 2 {
		t.Fatalf("sent %d requests, want 2", requests.Load())
	}
}

func TestInitializeAzureKeyVault(t *testing.T) {
	secretFile := t.TempDir() + "/azure-client-secret"
	if err := os.WriteFile(secretFile, []byte("test-client-secret"), 0o600); err != nil {
		t.Fatalf("write client secret file: %v", err)
	}

	t.Setenv("SECRET_PROVIDER_VAULT_URL", "https://example.vault.azure.net")
	t.Setenv("AZURE_AUTHORITY_HOST", "https://login.microsoftonline.com/")
	t.Setenv("AZURE_TENANT_ID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("AZURE_CLIENT_ID", "22222222-2222-2222-2222-222222222222")
	t.Setenv("AZURE_CLIENT_SECRET", "")
	t.Setenv("AZURE_CLIENT_SECRET_FILE", secretFile)

	provider, err := secretprovider.Initialize(t.Context(), azurekeyvault.Name, "v0.0.0-test")
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	t.Cleanup(provider.Close)

	if got := provider.Name(); got != azurekeyvault.Name {
		t.Fatalf("provider.Name() = %q, want %q", got, azurekeyvault.Name)
	}
}
