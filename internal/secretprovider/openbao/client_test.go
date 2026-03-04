package openbao

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/pkg/stdcopy"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
	"github.com/kimdre/doco-cd/internal/test"
)

var rootCredentials = struct {
	username string
	password string
}{
	username: "root",
	password: "root123",
}

var testCredentials = struct {
	username string
	password string
}{
	username: "test",
	password: "test123",
}

// setupOpenBaoContainers sets up the OpenBao test containers and returns the site URL and access token.
func setupOpenBaoContainers(t *testing.T) (siteUrl, accessToken string) {
	t.Helper()
	t.Log("starting OpenBao test container")

	ctx := context.Background()

	stack := test.ComposeUp(ctx, t, test.WithFile(filepath.Join("testdata", "openbao.compose.yml")))

	// Get the randomized host port mapped to Vault's default port 8200
	mappedPort := stack.MappedPort(ctx, t, "vault", "8200")

	// Initialize Vault
	exitStatus, output := stack.Exec(ctx, t, "vault", []string{"vault", "operator", "init", "-key-shares=1", "-key-threshold=1", "-format=json"})
	if exitStatus != 0 {
		t.Fatalf("vault init command failed with exit code %d", exitStatus)
	}

	var stdout, stderr bytes.Buffer

	_, err := stdcopy.StdCopy(&stdout, &stderr, output)
	if err != nil {
		t.Fatalf("failed to demultiplex vault init output: %v", err)
	}

	type InitOutput struct {
		RootToken  string   `json:"root_token"`
		UnsealKeys []string `json:"unseal_keys_b64"`
	}

	var initData InitOutput

	err = json.Unmarshal(stdout.Bytes(), &initData)
	if err != nil {
		t.Fatalf("failed to unmarshal vault init output: %v", err)
	}

	// Unseal Vault
	exitStatus, _ = stack.Exec(ctx, t, "vault", []string{"vault", "operator", "unseal", initData.UnsealKeys[0]})
	if exitStatus != 0 {
		t.Fatalf("failed to unseal vault (exit code %d)", exitStatus)
	}

	// Login to Vault
	exitStatus, _ = stack.Exec(ctx, t, "vault", []string{"vault", "login", initData.RootToken})
	if exitStatus != 0 {
		t.Fatalf("failed to login to vault (exit code %d)", exitStatus)
	}

	// Enable KV secrets engine at "rootSecret/" in root namespace
	exitStatus, _ = stack.Exec(ctx, t, "vault", []string{"vault", "secrets", "enable", "-path=rootSecret", "kv-v2"})
	if exitStatus != 0 {
		t.Fatalf("failed to enable kv secrets engine (exit code %d)", exitStatus)
	}

	// Enable PKI secrets engine at "pki/"
	exitStatus, _ = stack.Exec(ctx, t, "vault", []string{"vault", "secrets", "enable", "-path=pki", "pki"})
	if exitStatus != 0 {
		t.Fatalf("failed to enable pki secrets engine (exit code %d)", exitStatus)
	}

	// Create root CA
	exitStatus, _ = stack.Exec(ctx, t, "vault", []string{"vault", "write", "pki/root/generate/internal", "common_name=example.com", "ttl=8760h"})
	if exitStatus != 0 {
		t.Fatalf("failed to create root CA (exit code %d)", exitStatus)
	}

	// Create a role to issue certificates
	exitStatus, _ = stack.Exec(ctx, t, "vault", []string{"vault", "write", "pki/roles/example-dot-com", "allowed_domains=example.com", "allow_subdomains=true", "max_ttl=72h"})
	if exitStatus != 0 {
		t.Fatalf("failed to create pki role (exit code %d)", exitStatus)
	}

	// Issue a test certificate
	exitStatus, _ = stack.Exec(ctx, t, "vault", []string{"vault", "write", "pki/issue/example-dot-com", "common_name=test.example.com", "ttl=24h"})
	if exitStatus != 0 {
		t.Fatalf("failed to issue test certificate (exit code %d)", exitStatus)
	}

	// Add test secrets to root namespace
	exitStatus, _ = stack.Exec(ctx, t, "vault", []string{"vault", "kv", "put", "rootSecret/creds", "password=" + rootCredentials.password, "username=" + rootCredentials.username})
	if exitStatus != 0 {
		t.Fatalf("failed to add test secret (exit code %d)", exitStatus)
	}

	// Create additional namespace "test"
	exitStatus, _ = stack.Exec(ctx, t, "vault", []string{"vault", "namespace", "create", "test"})
	if exitStatus != 0 {
		t.Fatalf("failed to create namespace (exit code %d)", exitStatus)
	}

	// Enable KV secrets engine at "testSecret/" in "test" namespace
	exitStatus, _ = stack.Exec(ctx, t, "vault", []string{"vault", "secrets", "enable", "-namespace=test", "-path=testSecret", "kv-v2"})
	if exitStatus != 0 {
		t.Fatalf("failed to enable kv secrets engine in namespace (exit code %d)", exitStatus)
	}

	// Add test secrets to "test" namespace
	exitStatus, _ = stack.Exec(ctx, t, "vault", []string{"vault", "kv", "put", "-namespace=test", "testSecret/creds", "password=" + testCredentials.password, "username=" + testCredentials.username})
	if exitStatus != 0 {
		t.Fatalf("failed to add test secret to namespace (exit code %d)", exitStatus)
	}

	t.Logf("OpenBao container setup complete")

	return "http://localhost:" + mappedPort, initData.RootToken
}

func TestProvider_GetSecret_OpenBao(t *testing.T) {
	siteUrl, accessToken := setupOpenBaoContainers(t)

	testCases := []struct {
		name      string
		secretRef string
		expectErr bool
	}{
		{
			name:      "Valid KV secret reference in default namespace",
			secretRef: "kv:rootSecret:creds:password", // #nosec G101
			expectErr: false,
		},
		{
			name:      "Valid KV secret reference in root namespace with slash",
			secretRef: "kv:/:rootSecret:creds:password", // #nosec G101
			expectErr: false,
		},
		{
			name:      "Valid KV secret reference in root namespace",
			secretRef: "kv:root:rootSecret:creds:password", // #nosec G101
			expectErr: false,
		},
		{
			name:      "Valid KV secret reference in test namespace",
			secretRef: "kv:test:testSecret:creds:password", // #nosec G101
			expectErr: false,
		},
		{
			name:      "Invalid secret reference missing parts",
			secretRef: "kv:rootSecret:creds", // #nosec G101
			expectErr: true,
		},
		{
			name:      "Non-existent secret",
			secretRef: "kv:rootSecret:invalid:password",
			expectErr: true,
		},
		{
			name:      "Non-existent namespace",
			secretRef: "kv:invalid:rootSecret:creds:password", // #nosec G101
			expectErr: true,
		},
		{
			name:      "Valid PKI cert reference",
			secretRef: "pki:pki:test.example.com", // #nosec G101
			expectErr: false,
		},
		{
			name:      "Invalid reference format",
			secretRef: "pki:pki",
			expectErr: true,
		},
		{
			name:      "Non-existent PKI cert",
			secretRef: "pki:pki:nonexistent.example.com", // #nosec G101
			expectErr: true,
		},
		{
			name:      "Invalid engine type",
			secretRef: "invalid:creds:password", // #nosec G101
			expectErr: true,
		},
	}

	provider, err := NewProvider(t.Context(), siteUrl, accessToken)
	if err != nil {
		t.Fatalf("Failed to create provider: %v", err)
	}

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

func TestProvider_ResolveSecretReferences_OpenBao(t *testing.T) {
	siteUrl, accessToken := setupOpenBaoContainers(t)

	testCases := []struct {
		name             string
		secretsToResolve map[string]string
		expectedResolved secrettypes.ResolvedSecrets
	}{
		{
			name: "Single secret from default namespace",
			secretsToResolve: map[string]string{
				"ROOT_PASSWORD": "kv:rootSecret:creds:password", // #nosec G101
			},
			expectedResolved: secrettypes.ResolvedSecrets{
				"ROOT_PASSWORD": rootCredentials.password,
			},
		},
		{
			name: "Multiple secrets from root namespace",
			secretsToResolve: map[string]string{
				"ROOT_PASSWORD": "kv:root:rootSecret:creds:password", // #nosec G101
				"ROOT_USERNAME": "kv:root:rootSecret:creds:username", // #nosec G101
			},
			expectedResolved: secrettypes.ResolvedSecrets{
				"ROOT_PASSWORD": rootCredentials.password,
				"ROOT_USERNAME": rootCredentials.username,
			},
		},
		{
			name: "Multiple secrets from test namespace",
			secretsToResolve: map[string]string{
				"TEST_PASSWORD": "kv:test:testSecret:creds:password", // #nosec G101
				"TEST_USERNAME": "kv:test:testSecret:creds:username", // #nosec G101
			},
			expectedResolved: secrettypes.ResolvedSecrets{
				"TEST_PASSWORD": testCredentials.password,
				"TEST_USERNAME": testCredentials.username,
			},
		},
		{
			name: "Multiple secrets from root and test namespace",
			secretsToResolve: map[string]string{
				"ROOT_PASSWORD": "kv:root:rootSecret:creds:password", // #nosec G101
				"TEST_PASSWORD": "kv:test:testSecret:creds:password", // #nosec G101
			},
			expectedResolved: secrettypes.ResolvedSecrets{
				"ROOT_PASSWORD": rootCredentials.password,
				"TEST_PASSWORD": testCredentials.password,
			},
		},
	}

	provider, err := NewProvider(t.Context(), siteUrl, accessToken)
	if err != nil {
		t.Fatalf("Failed to create provider: %v", err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

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

func TestProvider_ResolveCertificate_OpenBao(t *testing.T) {
	siteUrl, accessToken := setupOpenBaoContainers(t)

	provider, err := NewProvider(t.Context(), siteUrl, accessToken)
	if err != nil {
		t.Fatalf("Failed to create provider: %v", err)
	}

	certRef := "pki:pki:test.example.com"

	cert, err := provider.GetSecret(t.Context(), certRef)
	if err != nil {
		t.Fatalf("Failed to get certificate: %v", err)
	}

	if cert == "" {
		t.Errorf("Expected a certificate value but got empty string")
	}

	// Check if the value looks like a PEM encoded certificate
	if !bytes.Contains([]byte(cert), []byte("-----BEGIN CERTIFICATE-----")) {
		t.Errorf("Expected PEM encoded certificate but got: %s", cert)
	}
}
