package openbao

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/docker/docker/pkg/stdcopy"
	"github.com/testcontainers/testcontainers-go/modules/compose"
	"github.com/testcontainers/testcontainers-go/wait"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

var testCredentials = struct {
	username string
	password string
}{
	username: "admin",
	password: "password123",
}

// setupOpenBaoContainers sets up the OpenBao test containers and returns the site URL and access token.
func setupOpenBaoContainers(t *testing.T) (siteUrl, accessToken string) {
	t.Log("starting OpenBao test container")

	ctx := context.Background()

	// Start OpenBao container, mounting bao.conf
	stack, err := compose.NewDockerCompose(filepath.Join("testdata", "openbao.compose.yml"))
	if err != nil {
		t.Fatalf("failed to create stack: %v", err)
	}

	err = stack.
		WaitForService("vault", wait.ForListeningPort("8200/tcp")).
		Up(ctx, compose.Wait(true))
	if err != nil {
		t.Fatalf("failed to start stack: %v", err)
	}

	t.Cleanup(func() {
		t.Log("stopping OpenBao test containers")

		if err = stack.Down(ctx,
			compose.RemoveOrphans(true),
			compose.RemoveVolumes(true),
			compose.RemoveImagesLocal); err != nil {
			t.Errorf("failed to stop stack: %v", err)
		}
	})

	// Initialize OpenBao with the provided configuration
	svc, err := stack.ServiceContainer(ctx, "vault")
	if err != nil {
		t.Fatalf("failed to get vault service container: %v", err)
	}

	// Initialize Vault
	exitStatus, output, err := svc.Exec(ctx, []string{"vault", "operator", "init", "-key-shares=1", "-key-threshold=1", "-format=json"})
	if err != nil {
		t.Fatalf("failed to initialize vault: %v", err)
	}

	if exitStatus != 0 {
		t.Fatalf("vault init command failed with exit code %d: %s", exitStatus, output)
	}

	var stdout, stderr bytes.Buffer

	_, err = stdcopy.StdCopy(&stdout, &stderr, output)
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
	exitStatus, _, err = svc.Exec(ctx, []string{"vault", "operator", "unseal", initData.UnsealKeys[0]})
	if err != nil || exitStatus != 0 {
		t.Fatalf("failed to unseal vault: %v", err)
	}

	// Login to Vault
	exitStatus, _, err = svc.Exec(ctx, []string{"vault", "login", initData.RootToken})
	if err != nil || exitStatus != 0 {
		t.Fatalf("failed to login to vault: %v", err)
	}

	// Enable KV secrets engine at "secret/"
	exitStatus, _, err = svc.Exec(ctx, []string{"vault", "secrets", "enable", "-path=secret", "kv-v2"})
	if err != nil || exitStatus != 0 {
		t.Fatalf("failed to enable kv secrets engine: %v", err)
	}

	// Enable PKI secrets engine at "pki/"
	exitStatus, _, err = svc.Exec(ctx, []string{"vault", "secrets", "enable", "-path=pki", "pki"})
	if err != nil || exitStatus != 0 {
		t.Fatalf("failed to enable pki secrets engine: %v", err)
	}

	// Create root CA
	exitStatus, _, err = svc.Exec(ctx, []string{"vault", "write", "pki/root/generate/internal", "common_name=example.com", "ttl=8760h"})
	if err != nil || exitStatus != 0 {
		t.Fatalf("failed to create root CA: %v", err)
	}

	// Create a role to issue certificates
	exitStatus, _, err = svc.Exec(ctx, []string{"vault", "write", "pki/roles/example-dot-com", "allowed_domains=example.com", "allow_subdomains=true", "max_ttl=72h"})
	if err != nil || exitStatus != 0 {
		t.Fatalf("failed to create pki role: %v", err)
	}

	// Issue a test certificate
	exitStatus, _, err = svc.Exec(ctx, []string{"vault", "write", "pki/issue/example-dot-com", "common_name=test.example.com", "ttl=24h"})
	if err != nil || exitStatus != 0 {
		t.Fatalf("failed to issue test certificate: %v", err)
	}

	// Add test secrets
	exitStatus, _, err = svc.Exec(ctx, []string{"vault", "kv", "put", "secret/testSecret", "password=" + testCredentials.password, "username=" + testCredentials.username})
	if err != nil || exitStatus != 0 {
		t.Fatalf("failed to add test secret: %v", err)
	}

	t.Logf("OpenBao container setup complete")

	return "http://localhost:8200", initData.RootToken
}

func TestProvider_GetSecret_OpenBao(t *testing.T) {
	siteUrl, accessToken := setupOpenBaoContainers(t)

	testCases := []struct {
		name      string
		secretRef string
		expectErr bool
	}{
		{
			name:      "Valid KV secret reference",
			secretRef: "kv:secret:testSecret:password",
			expectErr: false,
		},
		{
			name:      "Invalid secret reference missing parts",
			secretRef: "kv:secret:testSecret",
			expectErr: true,
		},
		{
			name:      "Non-existent secret",
			secretRef: "kv:secret:invalid:password",
			expectErr: true,
		},
		{
			name:      "Valid PKI cert reference",
			secretRef: "pki:pki:test.example.com",
			expectErr: false,
		},
		{
			name:      "Invalid reference format",
			secretRef: "pki:pki",
			expectErr: true,
		},
		{
			name:      "Non-existent PKI cert",
			secretRef: "pki:pki:nonexistent.example.com",
			expectErr: true,
		},
		{
			name:      "Invalid engine type",
			secretRef: "invalid:testSecret:password",
			expectErr: true,
		},
	}

	provider, err := NewProvider(t.Context(), siteUrl, accessToken)
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

func TestProvider_ResolveSecretReferences_OpenBao(t *testing.T) {
	siteUrl, accessToken := setupOpenBaoContainers(t)

	testCases := []struct {
		name             string
		secretsToResolve map[string]string
		expectedResolved secrettypes.ResolvedSecrets
	}{
		{
			name: "Single secret",
			secretsToResolve: map[string]string{
				"TEST_PASSWORD": "kv:secret:testSecret:password",
			},
			expectedResolved: secrettypes.ResolvedSecrets{
				"TEST_PASSWORD": testCredentials.password,
			},
		},
		{
			name: "Multiple secrets",
			secretsToResolve: map[string]string{
				"TEST_PASSWORD": "kv:secret:testSecret:password",
				"TEST_USERNAME": "kv:secret:testSecret:username",
			},
			expectedResolved: secrettypes.ResolvedSecrets{
				"TEST_PASSWORD": testCredentials.password,
				"TEST_USERNAME": testCredentials.username,
			},
		},
	}

	provider, err := NewProvider(t.Context(), siteUrl, accessToken)
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
