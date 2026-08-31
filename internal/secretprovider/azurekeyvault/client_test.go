package azurekeyvault

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

type fakeSecretClient struct {
	name     string
	version  string
	response azsecrets.GetSecretResponse
	err      error
}

func (f *fakeSecretClient) GetSecret(
	_ context.Context,
	name, version string,
	_ *azsecrets.GetSecretOptions,
) (azsecrets.GetSecretResponse, error) {
	f.name = name
	f.version = version

	return f.response, f.err
}

func TestValueProviderGetSecret(t *testing.T) {
	value := "secret-value"

	for _, test := range []struct {
		name        string
		ref         string
		wantName    string
		wantVersion string
	}{
		{
			name:     "latest version",
			ref:      "database-password",
			wantName: "database-password",
		},
		{
			name:        "pinned version",
			ref:         "database-password/7c3cc89647ba4b9f8dd7d8d5f92d00b9",
			wantName:    "database-password",
			wantVersion: "7c3cc89647ba4b9f8dd7d8d5f92d00b9",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeSecretClient{
				response: azsecrets.GetSecretResponse{
					Secret: azsecrets.Secret{Value: &value},
				},
			}
			provider := newValueProvider(client)

			got, err := provider.GetSecret(t.Context(), test.ref)
			if err != nil {
				t.Fatalf("GetSecret() error = %v", err)
			}

			if got != value {
				t.Fatalf("GetSecret() = %q, want %q", got, value)
			}

			if client.name != test.wantName {
				t.Fatalf("Azure secret name = %q, want %q", client.name, test.wantName)
			}

			if client.version != test.wantVersion {
				t.Fatalf("Azure secret version = %q, want %q", client.version, test.wantVersion)
			}
		})
	}
}

func TestValueProviderGetSecretRejectsInvalidReference(t *testing.T) {
	for _, ref := range []string{"", "/version", "name/", "name/version/extra"} {
		t.Run(ref, func(t *testing.T) {
			provider := newValueProvider(&fakeSecretClient{})

			_, err := provider.GetSecret(t.Context(), ref)
			if !errors.Is(err, ErrInvalidSecretReference) {
				t.Fatalf("GetSecret() error = %v, want ErrInvalidSecretReference", err)
			}
		})
	}
}

func TestValueProviderGetSecretPropagatesClientError(t *testing.T) {
	clientErr := errors.New("azure request failed")
	provider := newValueProvider(&fakeSecretClient{err: clientErr})

	_, err := provider.GetSecret(t.Context(), "database-password")
	if !errors.Is(err, clientErr) {
		t.Fatalf("GetSecret() error = %v, want client error", err)
	}
}

func TestValueProviderGetSecretRejectsMissingValue(t *testing.T) {
	provider := newValueProvider(&fakeSecretClient{})

	_, err := provider.GetSecret(t.Context(), "database-password")
	if !errors.Is(err, ErrSecretValueMissing) {
		t.Fatalf("GetSecret() error = %v, want ErrSecretValueMissing", err)
	}
}

func TestNewCredentialUsesClientSecretCredentialForSecretFile(t *testing.T) {
	cfg := &Config{
		TenantID:             "11111111-1111-1111-1111-111111111111",
		ClientID:             "22222222-2222-2222-2222-222222222222",
		ClientSecret:         "client-secret",
		clientSecretFromFile: true,
	}

	credential, err := newCredential(cfg)
	if err != nil {
		t.Fatalf("newCredential() error = %v", err)
	}

	if _, ok := credential.(*azidentity.ClientSecretCredential); !ok {
		t.Fatalf("newCredential() type = %T, want *azidentity.ClientSecretCredential", credential)
	}
}

func TestNewCredentialRejectsIncompleteSecretFileCredentials(t *testing.T) {
	cfg := &Config{
		ClientSecret:         "client-secret",
		clientSecretFromFile: true,
	}

	if _, err := newCredential(cfg); err == nil {
		t.Fatal("newCredential() error = nil, want error")
	}
}
