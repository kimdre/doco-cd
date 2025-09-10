package secrets

import (
	"path/filepath"

	"github.com/bitwarden/sdk-go"
)

type BitwardenSecretsManagerProvider struct {
	Client sdk.BitwardenClientInterface
}

// NewBitwardenSecretsManagerProvider creates a new BitwardenSecretsManagerProvider instance and performs login using the provided access token.
func NewBitwardenSecretsManagerProvider(apiUrl, identityURL, accessToken string) (*BitwardenSecretsManagerProvider, error) {
	client, err := sdk.NewBitwardenClient(&apiUrl, &identityURL)
	if err != nil {
		return nil, err
	}

	provider := &BitwardenSecretsManagerProvider{Client: client}

	stateFileName, err := filepath.Abs("~/bitwarden-sm-state.json")
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

// GetSecret retrieves a secret value from Bitwarden Secrets Manager using the provided secret ID.
func (b *BitwardenSecretsManagerProvider) GetSecret(id string) (string, error) {
	secret, err := b.Client.Secrets().Get(id)
	if err != nil {
		return "", err
	}

	return secret.Value, nil
}

// GetSecrets retrieves multiple secrets from Bitwarden Secrets Manager using the provided list of secret IDs.
func (b *BitwardenSecretsManagerProvider) GetSecrets(ids []string) (map[string]string, error) {
	secrets, err := b.Client.Secrets().GetByIDS(ids)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for _, secret := range secrets.Data {
		result[secret.ID] = secret.Value
	}

	return result, nil
}

// Close cleans up resources used by the BitwardenSecretsManagerProvider.
func (b *BitwardenSecretsManagerProvider) Close() {
	b.Client.Close()
}
