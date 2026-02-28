package awssecretsmanager

import (
	"reflect"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
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

func TestProvider_GetSecret_AWSSecretManager(t *testing.T) {
	skipWrongProvider(t)

	t.Parallel()

	secretARN := "arn:aws:secretsmanager:eu-west-1:243238513853:secret:test-RAbPpz" // #nosec G101
	expectedValue := "{\"username\":\"ulli\",\"password\":\"irgendwas\"}"

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("Failed to get config: %v", err)
	}

	provider, err := NewProvider(t.Context(), cfg.Region, cfg.AccessKeyID, cfg.SecretAccessKey)
	if err != nil {
		t.Fatalf("Failed to create provider: %v", err)
	}

	secret, err := provider.GetSecret(t.Context(), secretARN)
	if err != nil {
		t.Fatalf("Failed to get secret: %v", err)
	}

	if secret != expectedValue {
		t.Errorf("Expected secret value %s, got %s", expectedValue, secret)
	}
}

func TestProvider_ResolveSecretReferences_AWSSecretManager(t *testing.T) {
	skipWrongProvider(t)

	t.Parallel()

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
			name: "Without Path in ARN",
			secretsToResolve: map[string]string{
				"TEST_SECRET": "arn:aws:secretsmanager:eu-west-1:243238513853:secret:test-RAbPpz", // #nosec G101
			},
			expectedResolved: secrettypes.ResolvedSecrets{
				"TEST_SECRET": "{\"username\":\"ulli\",\"password\":\"something\",\"with/delimiter\":\"some/value\"}", // #nosec G101
			},
		},
		{
			name: "With Path in ARN",
			secretsToResolve: map[string]string{
				"TEST_SECRET":    "arn:aws:secretsmanager:eu-west-1:243238513853:secret:test-RAbPpz/password", // #nosec G101
				"USERNAME":       "arn:aws:secretsmanager:eu-west-1:243238513853:secret:test-RAbPpz/username",
				"WITH_DELIMITER": "arn:aws:secretsmanager:eu-west-1:243238513853:secret:test-RAbPpz/with/delimiter",
			},
			expectedResolved: secrettypes.ResolvedSecrets{
				"TEST_SECRET":    "something",
				"USERNAME":       "ulli",
				"WITH_DELIMITER": "some/value",
			},
		},
	}

	provider, err := NewProvider(t.Context(), cfg.Region, cfg.AccessKeyID, cfg.SecretAccessKey)
	if err != nil {
		t.Fatalf("Failed to create provider: %v", err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resolvedSecrets, err := provider.ResolveSecretReferences(t.Context(), tc.secretsToResolve)
			if err != nil {
				t.Fatalf("Failed to resolve secret references: %v", err)
			}

			if len(resolvedSecrets) != len(tc.expectedResolved) {
				t.Fatalf("Expected %d resolved secrets, got %d", len(tc.expectedResolved), len(resolvedSecrets))
			}

			// Compare expectedResolved with resolvedSecrets
			if !reflect.DeepEqual(tc.expectedResolved, resolvedSecrets) {
				t.Fatalf("Resolved secrets do not match expected values.\nExpected: %v\nGot: %v", tc.expectedResolved, resolvedSecrets)
			}
		})
	}
}
