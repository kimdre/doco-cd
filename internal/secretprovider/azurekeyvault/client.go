package azurekeyvault

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

const Name = "azure_kv"

var (
	ErrInvalidSecretReference = errors.New("invalid secret reference")
	ErrSecretValueMissing     = errors.New("secret value missing")
)

type secretClient interface {
	GetSecret(ctx context.Context, name, version string, options *azsecrets.GetSecretOptions) (azsecrets.GetSecretResponse, error)
}

type ValueProvider struct {
	client secretClient
}

// NewValueProvider creates a provider using Azure's default credential chain.
func NewValueProvider(vaultURL string) (*ValueProvider, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure credential: %w", err)
	}

	client, err := azsecrets.NewClient(vaultURL, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure Key Vault client: %w", err)
	}

	return newValueProvider(client), nil
}

func newValueProvider(client secretClient) *ValueProvider {
	return &ValueProvider{client: client}
}

// GetSecret retrieves an Azure Key Vault secret by name and optional version.
func (p *ValueProvider) GetSecret(ctx context.Context, ref string) (string, error) {
	name, version, err := parseSecretReference(ref)
	if err != nil {
		return "", err
	}

	response, err := p.client.GetSecret(ctx, name, version, nil)
	if err != nil {
		return "", err
	}

	if response.Value == nil {
		return "", fmt.Errorf("%w: %s", ErrSecretValueMissing, ref)
	}

	return *response.Value, nil
}

func parseSecretReference(ref string) (name, version string, err error) {
	parts := strings.Split(ref, "/")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") {
		return "", "", fmt.Errorf("%w: expected 'name' or 'name/version'", ErrInvalidSecretReference)
	}

	if len(parts) == 2 {
		version = parts[1]
	}

	return parts[0], version, nil
}
