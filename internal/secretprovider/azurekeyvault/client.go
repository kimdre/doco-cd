package azurekeyvault

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
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
	NewListSecretPropertiesPager(options *azsecrets.ListSecretPropertiesOptions) *runtime.Pager[azsecrets.ListSecretPropertiesResponse]
}

type ValueProvider struct {
	client secretClient
}

// NewValueProvider creates an Azure Key Vault value provider.
func NewValueProvider(cfg *Config) (*ValueProvider, error) {
	credential, err := newCredential(cfg)
	if err != nil {
		return nil, fmt.Errorf("create Azure credential: %w", err)
	}

	client, err := azsecrets.NewClient(string(cfg.VaultURL), credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure Key Vault client: %w", err)
	}

	return newValueProvider(client), nil
}

func newCredential(cfg *Config) (azcore.TokenCredential, error) {
	if cfg.clientSecretFromFile {
		return azidentity.NewClientSecretCredential(cfg.TenantID, cfg.ClientID, cfg.ClientSecret, nil)
	}

	return azidentity.NewDefaultAzureCredential(nil)
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

// GetSecretsByPath retrieves all enabled secrets whose name starts with the given path.
// Key Vault has no folders, so the path is a name prefix. The path "/" selects all secrets.
// The returned names have the prefix removed. Key Vault names cannot contain a slash,
// so the recursive option has no effect.
func (p *ValueProvider) GetSecretsByPath(ctx context.Context, path string, _ bool) (map[string]string, error) {
	namePrefix := strings.TrimPrefix(path, "/")

	pager := p.client.NewListSecretPropertiesPager(nil)
	out := make(map[string]string)

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, properties := range page.Value {
			if properties == nil || properties.ID == nil {
				continue
			}

			if properties.Attributes != nil && properties.Attributes.Enabled != nil && !*properties.Attributes.Enabled {
				continue
			}

			name := properties.ID.Name()
			if !hasPrefixFold(name, namePrefix) || len(name) == len(namePrefix) {
				continue
			}

			value, err := p.GetSecret(ctx, name)
			if err != nil {
				return nil, err
			}

			out[name[len(namePrefix):]] = value
		}
	}

	return out, nil
}

// hasPrefixFold reports whether name starts with prefix. Key Vault names are case-insensitive,
// so the comparison ignores case.
func hasPrefixFold(name, prefix string) bool {
	return len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix)
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
