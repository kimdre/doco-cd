package bitwardensecretsmanager

import (
	"path/filepath"

	"github.com/bitwarden/sdk-go"
)

const (
	ProviderName  = "bitwarden_sm"
	stateFileName = "~/bitwarden-sm-state.json"
)

type Provider struct {
	Client sdk.BitwardenClientInterface
}

func (p *Provider) Provider() string {
	return ProviderName
}

// NewProvider creates a new Provider instance for Bitwarden Secrets Manager and performs login using the provided access token.
func NewProvider(apiUrl, identityURL, accessToken string) (*Provider, error) {
	client, err := sdk.NewBitwardenClient(&apiUrl, &identityURL)
	if err != nil {
		return nil, err
	}

	provider := &Provider{Client: client}

	stateFileName, err := filepath.Abs(stateFileName)
	if err != nil {
		return nil, err
	}

	// Perform login with the provided access token
	err = provider.Client.AccessTokenLogin(accessToken, &stateFileName)
	if err != nil {
		return nil, err
	}

	return provider, nil
}

// GetSecret retrieves a secret value from the Bitwarden Secrets Manager using the provided secret ID.
func (p *Provider) GetSecret(id string) (string, error) {
	secret, err := p.Client.Secrets().Get(id)
	if err != nil {
		return "", err
	}

	return secret.Value, nil
}

// GetSecrets retrieves multiple secrets from Bitwarden Secrets Manager using the provided list of secret IDs.
func (p *Provider) GetSecrets(ids []string) (map[string]string, error) {
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

// Close cleans up resources used by the Provider.
func (p *Provider) Close() {
	p.Client.Close()
}
