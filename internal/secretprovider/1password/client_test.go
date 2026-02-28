package onepassword

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
)

func skipWrongProvider(t *testing.T) {
	t.Helper()

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("unable to get app config: %v", err)
	}

	if c.SecretProvider != Name {
		t.Skipf("Skipping provider tests since SECRET_PROVIDER is not set to '%s'", Name)
	}
}

// TestProvider_GetSecret_OnePassword tests the GetSecret method of the 1Password Provider.
func TestProvider_GetSecret_OnePassword(t *testing.T) {
	skipWrongProvider(t)

	t.Parallel()

	testCases := []struct {
		name      string
		secretRef string
		expectErr bool
	}{
		{
			name:      "Valid secret reference",
			secretRef: "op://Doco-CD/Secret Test/OTHER_SECRET", // #nosec G101
			expectErr: false,
		},
		{
			name:      "Invalid secret reference missing parts",
			secretRef: "op://Doco-CD/Secret Test", // #nosec G101
			expectErr: true,
		},
		{
			name:      "Non-existent secret",
			secretRef: "op://Doco-CD/Secret Test/NON_EXISTENT_SECRET", // #nosec G101
			expectErr: true,
		},
	}

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("unable to get config: %v", err)
	}

	provider, err := NewProvider(t.Context(), cfg.AccessToken, "test")
	if err != nil {
		t.Fatalf("Failed to create OnePassword provider: %v", err)
	}

	t.Cleanup(func() {
		provider.Close()
	})

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

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
