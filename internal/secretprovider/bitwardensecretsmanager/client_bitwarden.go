//go:build !nobitwarden

package bitwardensecretsmanager

import (
	"context"
	"path/filepath"

	"github.com/bitwarden/sdk-go"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const (
	Name          = "bitwarden_sm"
	stateFilePath = "/tmp/bitwarden-sm-state.json"
)

type Provider struct {
	Client sdk.BitwardenClientInterface
}

// Name returns the name of the secret provider.
func (p *Provider) Name() string {
	return Name
}

// NewProvider creates a new Provider instance for Bitwarden Secrets Manager and performs login using the provided access token.
func NewProvider(apiUrl, identityURL, accessToken string) (*Provider, error) {
	client, err := sdk.NewBitwardenClient(&apiUrl, &identityURL)
	if err != nil {
		return nil, err
	}

	provider := &Provider{Client: client}

	stateFile, err := filepath.Abs(stateFilePath)
	if err != nil {
		return nil, err
	}

	// Perform login with the provided access token
	err = provider.Client.AccessTokenLogin(accessToken, &stateFile)
	if err != nil {
		return nil, err
	}

	return provider, nil
}

// GetSecret retrieves a secret value from the Bitwarden Secrets Manager using the provided secret ID.
func (p *Provider) GetSecret(_ context.Context, id string) (string, error) {
	secret, err := p.Client.Secrets().Get(id)
	if err != nil {
		return "", err
	}

	return secret.Value, nil
}

// GetSecrets retrieves multiple secrets from Bitwarden Secrets Manager using the provided list of secret IDs.
func (p *Provider) GetSecrets(_ context.Context, ids []string) (map[string]string, error) {
	secrets, err := p.Client.Secrets().GetByIDS(ids)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for _, secret := range secrets.Data {
		result[secret.ID] = secret.Value
	}

	return result, nil
}

// ResolveSecretReferences resolves the provided map of environment variable names to secret IDs
// by fetching the corresponding secret values from the secret provider.
func (p *Provider) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
	ids := make([]string, 0, len(secrets))
	for _, id := range secrets {
		ids = append(ids, id)
	}

	resolved, err := p.GetSecrets(ctx, ids)
	if err != nil {
		return nil, err
	}

	for envVar, secretID := range secrets {
		if val, ok := resolved[secretID]; ok {
			secrets[envVar] = val
		}
	}

	return secrets, nil
}

// Close cleans up resources used by the Provider.
func (p *Provider) Close() {
	p.Client.Close()
}
