package bitwardensecretsmanager

import (
	"testing"

	"github.com/bitwarden/sdk-go"

	"github.com/kimdre/doco-cd/internal/config"
)

const (
	validSecretID   = "138e3697-ed58-431c-b866-b3550066343a" // #nosec G101
	wrongSecretID   = "c42b74b2-1cde-45ef-83fe-19d86240ef47" // #nosec G101
	invalidSecretID = "invalid-secret-id"
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

func TestNewProvider_BitwardenSecretManager(t *testing.T) {
	skipWrongProvider(t)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("unable to get config: %v", err)
	}

	testCases := []struct {
		name        string
		apiUrl      string
		identityURL string
		accessToken string
		expectError string
	}{
		{
			name:        "Valid parameters",
			apiUrl:      cfg.ApiUrl,
			identityURL: cfg.IdentityUrl,
			accessToken: cfg.AccessToken,
			expectError: "",
		},
		{
			name:        "Invalid API URL",
			apiUrl:      "invalid-url",
			identityURL: cfg.IdentityUrl,
			accessToken: cfg.AccessToken,
			expectError: "",
		},
		{
			name:        "Empty Access Token",
			apiUrl:      cfg.ApiUrl,
			identityURL: cfg.IdentityUrl,
			accessToken: "",
			expectError: "API error: Access token is not in a valid format: Doesn't contain a decryption key",
		},
		{
			name:        "Invalid Access Token",
			apiUrl:      cfg.ApiUrl,
			identityURL: cfg.IdentityUrl,
			accessToken: "invalid-token",
			expectError: "API error: Access token is not in a valid format: Doesn't contain a decryption key",
		},
		{
			name:        "Empty API URL",
			apiUrl:      "",
			identityURL: cfg.IdentityUrl,
			accessToken: cfg.AccessToken,
			expectError: "",
		},
		{
			name:        "Empty Identity URL",
			apiUrl:      cfg.ApiUrl,
			identityURL: "",
			accessToken: cfg.AccessToken,
			expectError: "API error: builder error",
		},
		{
			name:        "Empty API and Identity URL",
			apiUrl:      "",
			identityURL: "",
			accessToken: cfg.AccessToken,
			expectError: "API error: builder error",
		},
	}

	var provider *Provider

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			provider, err = NewProvider(tc.apiUrl, tc.identityURL, tc.accessToken)
			if err != nil {
				if tc.expectError == "" {
					t.Fatalf("Unexpected error: %v", err)
				} else if err.Error() != tc.expectError {
					t.Fatalf("Expected error: %v, but got: %v", tc.expectError, err)
				}

				return
			}

			if provider == nil {
				t.Fatal("Expected provider to be non-nil")
			}

			if provider.Client == nil {
				t.Fatal("Expected provider.Client to be non-nil")
			}

			var project *sdk.ProjectResponse

			project, err = provider.Client.Projects().Get("1f60dcc3-4522-4095-b8e3-b3550065fef8")
			if err != nil {
				return
			}

			if project == nil {
				t.Fatal("Expected list to be non-nil")
			}

			t.Cleanup(func() {
				provider.Close()
			})
		})
	}
}

func TestProvider_GetSecret_BitwardenSecretManager(t *testing.T) {
	skipWrongProvider(t)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("unable to get config: %v", err)
	}

	provider, err := NewProvider(cfg.ApiUrl, cfg.IdentityUrl, cfg.AccessToken)
	if err != nil {
		t.Fatalf("Failed to create Bitwarden provider: %v", err)
	}

	t.Cleanup(func() {
		provider.Close()
	})

	testCases := []struct {
		name        string
		secretID    string
		expectError string
	}{
		{
			name:        "Valid Secret ID",
			secretID:    validSecretID,
			expectError: "",
		},
		{
			name:        "Invalid Secret ID",
			secretID:    invalidSecretID,
			expectError: "API error: Invalid command value: UUID parsing failed: invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `i` at 1",
		},
		{
			name:        "Wrong Secret ID",
			secretID:    wrongSecretID,
			expectError: " API error: Received error message from server: [404 Not Found] {\"message\":\"Resource not found.\",\"validationErrors\":null,\"exceptionMessage\":null,\"exceptionStackTrace\":null,\"innerExceptionMessage\":null,\"object\":\"error\"}",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			secretValue, err := provider.GetSecret(t.Context(), tc.secretID)
			if tc.expectError != "" {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					if err.Error() != tc.expectError {
						t.Errorf("Expected error: %v, but got: %v", tc.expectError, err)
					}
				}

				if secretValue == "" {
					t.Errorf("Expected non-empty secret value")
				}
			}
		})
	}
}

func TestProvider_GetSecrets_BitwardenSecretManager(t *testing.T) {
	skipWrongProvider(t)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("unable to get config: %v", err)
	}

	provider, err := NewProvider(cfg.ApiUrl, cfg.IdentityUrl, cfg.AccessToken)
	if err != nil {
		t.Fatalf("Failed to create Bitwarden provider: %v", err)
	}

	t.Cleanup(func() {
		provider.Close()
	})

	testCases := []struct {
		name        string
		secretIDs   []string
		expectError string
	}{
		{
			name:        "Valid Secret IDs",
			secretIDs:   []string{validSecretID},
			expectError: "",
		},
		{
			name:        "Valid and wrong Secret ID",
			secretIDs:   []string{validSecretID, wrongSecretID},
			expectError: "API error: Received error message from server: [404 Not Found] {\"message\":\"Resource not found.\",\"validationErrors\":null,\"exceptionMessage\":null,\"exceptionStackTrace\":null,\"innerExceptionMessage\":null,\"object\":\"error\"}",
		},
		{
			name:        "One Invalid Secret ID",
			secretIDs:   []string{validSecretID, invalidSecretID},
			expectError: "API error: Invalid command value: UUID parsing failed: invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `i` at 1",
		},
		{
			name:        "All Invalid Secret IDs",
			secretIDs:   []string{invalidSecretID + "1", invalidSecretID + "2"},
			expectError: "API error: Invalid command value: UUID parsing failed: invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `i` at 1",
		},
		{
			name:        "Empty Secret IDs",
			secretIDs:   []string{},
			expectError: "API error: Received error message from server: [404 Not Found] {\"message\":\"Resource not found.\",\"validationErrors\":null,\"exceptionMessage\":null,\"exceptionStackTrace\":null,\"innerExceptionMessage\":null,\"object\":\"error\"}",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			secrets, err := provider.GetSecrets(t.Context(), tc.secretIDs)
			if tc.expectError != "" {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					if err.Error() != tc.expectError {
						t.Errorf("Expected error: %v, but got: %v", tc.expectError, err)
					}
				}

				if len(secrets) == 0 && len(tc.secretIDs) > 0 {
					t.Errorf("Expected non-empty secrets map")
				}

				for id, val := range secrets {
					if val == "" {
						t.Errorf("Expected non-empty secret value")
					}

					t.Logf("Retrieved secret ID: %s, Value: %s", id, val)
				}
			}
		})
	}
}

func TestProvider_ResolveSecretReferences_BitwardenSecretManager(t *testing.T) {
	skipWrongProvider(t)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("unable to get config: %v", err)
	}

	provider, err := NewProvider(cfg.ApiUrl, cfg.IdentityUrl, cfg.AccessToken)
	if err != nil {
		t.Fatalf("Failed to create Bitwarden provider: %v", err)
	}

	t.Cleanup(func() {
		provider.Close()
	})

	testCases := []struct {
		name        string
		secrets     map[string]string
		expectError string
	}{
		{
			name: "Valid Secret References",
			secrets: map[string]string{
				"DB_PASSWORD": validSecretID,
			},
			expectError: "",
		},
		{
			name: "Valid and wrong Secret References",
			secrets: map[string]string{
				"DB_PASSWORD": validSecretID,
				"API_KEY":     wrongSecretID,
			},
			expectError: "API error: Received error message from server: [404 Not Found] {\"message\":\"Resource not found.\",\"validationErrors\":null,\"exceptionMessage\":null,\"exceptionStackTrace\":null,\"innerExceptionMessage\":null,\"object\":\"error\"}",
		},
		{
			name: "One Invalid Secret Reference",
			secrets: map[string]string{
				"DB_PASSWORD": validSecretID,
				"API_KEY":     invalidSecretID,
			},
			expectError: "API error: Invalid command value: UUID parsing failed: invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `i` at 1",
		},
		{
			name: "All Invalid Secret References",
			secrets: map[string]string{
				"DB_PASSWORD": invalidSecretID + "1",
				"API_KEY":     invalidSecretID + "2",
			},
			expectError: "API error: Invalid command value: UUID parsing failed: invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `i` at 1",
		},
		{
			name:        "Empty Secret References",
			secrets:     map[string]string{},
			expectError: "API error: Received error message from server: [404 Not Found] {\"message\":\"Resource not found.\",\"validationErrors\":null,\"exceptionMessage\":null,\"exceptionStackTrace\":null,\"innerExceptionMessage\":null,\"object\":\"error\"}",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			resolvedSecrets, err := provider.ResolveSecretReferences(t.Context(), tc.secrets)
			if tc.expectError != "" {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					if err.Error() != tc.expectError {
						t.Errorf("Expected error: %v, but got: %v", tc.expectError, err)
					}
				}

				if len(resolvedSecrets) == 0 && len(tc.secrets) > 0 {
					t.Errorf("Expected non-empty resolved secrets map")
				}

				for key, val := range resolvedSecrets {
					if val == "" {
						t.Errorf("Expected non-empty secret value for key: %s", key)
					}

					t.Logf("Resolved secret key: %s, Value: %s", key, val)
				}
			}
		})
	}
}
