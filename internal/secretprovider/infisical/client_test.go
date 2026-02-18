package infisical

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

func skipWrongProvider(t *testing.T) {
	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("unable to get app config: %v", err)
	}

	if c.SecretProvider != Name {
		t.Skipf("Skipping provider tests since SECRET_PROVIDER is not set to '%s'", Name)
	}
}

func TestProvider_GetSecret_Infisical(t *testing.T) {
	skipWrongProvider(t)

	testCases := []struct {
		name      string
		secretRef string
		expectErr bool
	}{
		{
			name:      "Valid secret reference basic",
			secretRef: "0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:DATABASE_URL", // #nosec G101
			expectErr: false,
		},
		{
			name:      "Valid secret reference with absolute path",
			secretRef: "0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:/Test/Sub/TEST_SECRET", // #nosec G101
			expectErr: false,
		},
		{
			name:      "Valid secret reference with relative path",
			secretRef: "0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:Test/Sub/TEST_SECRET", // #nosec G101
			expectErr: false,
		},
		{
			name:      "Invalid secret reference missing parts",
			secretRef: "0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev", // #nosec G101
			expectErr: true,
		},
		{
			name:      "Non-existent secret",
			secretRef: "0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:NON_EXISTENT_SECRET", // #nosec G101
			expectErr: true,
		},
	}

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("Failed to get config: %v", err)
	}

	provider, err := NewProvider(t.Context(), cfg.SiteUrl, cfg.ClientID, cfg.ClientSecret)
	if err != nil {
		t.Fatalf("Failed to create provider: %v", err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			secret, err := provider.GetSecret(t.Context(), tc.secretRef)
			if tc.expectErr && err == nil {
				t.Errorf("Expected error but got none")
			}

			if !tc.expectErr && err != nil {
				t.Errorf("Did not expect error but got: %v", err)
			}

			if !tc.expectErr && secret == "" {
				t.Errorf("Expected a secret value but got empty string")
			}
		})
	}
}

func TestProvider_ResolveSecretReferences_Infisical(t *testing.T) {
	skipWrongProvider(t)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("Failed to get config: %v", err)
	}

	testCases := []struct {
		name             string
		secretsToResolve map[string]string
		expectedResolved secrettypes.ResolvedSecrets
	}{
		{
			name: "Single secret",
			secretsToResolve: map[string]string{
				"TEST_KEY": "0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:TEST_KEY",
			},
			expectedResolved: secrettypes.ResolvedSecrets{
				"TEST_KEY": "test-pass",
			},
		},
		{
			name: "Multiple secrets",
			secretsToResolve: map[string]string{
				"TEST_KEY":    "0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:TEST_KEY",
				"TEST_SECRET": "0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:/Test/Sub/TEST_SECRET", // #nosec G101
			},
			expectedResolved: secrettypes.ResolvedSecrets{
				"TEST_KEY":    "test-pass",
				"TEST_SECRET": "test-value",
			},
		},
	}

	provider, err := NewProvider(t.Context(), cfg.SiteUrl, cfg.ClientID, cfg.ClientSecret)
	if err != nil {
		t.Fatalf("Failed to create provider: %v", err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := provider.ResolveSecretReferences(t.Context(), tc.secretsToResolve)
			if err != nil {
				t.Fatalf("Failed to resolve secrets: %v", err)
			}

			for key, expectedValue := range tc.expectedResolved {
				if resolved[key] != expectedValue {
					t.Errorf("For key %s, expected value %s but got %s", key, expectedValue, resolved[key])
				}
			}
		})
	}
}
