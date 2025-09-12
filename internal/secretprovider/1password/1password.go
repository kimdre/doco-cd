package onepassword

import (
	"context"
	"fmt"

	"github.com/1password/onepassword-sdk-go"
)

const (
	Name = "1password"
)

type Provider struct {
	Client onepassword.Client
}

func (p *Provider) Name() string {
	return Name
}

// NewProvider creates a new Provider instance for 1Password and performs login using the provided service account token.
func NewProvider(ctx context.Context, accessToken, version string) (*Provider, error) {
	client, err := onepassword.NewClient(
		ctx,
		onepassword.WithServiceAccountToken(accessToken),
		onepassword.WithIntegrationInfo("doco-cd", version),
	)
	if err != nil {
		return nil, err
	}

	provider := &Provider{Client: *client}

	return provider, nil
}

// GetSecret retrieves a secret value from 1Password using the provided URI.
func (p *Provider) GetSecret(ctx context.Context, uri string) (string, error) {
	if err := onepassword.Secrets.ValidateSecretReference(ctx, uri); err != nil {
		return "", err
	}

	secret, err := p.Client.Secrets().Resolve(ctx, uri)
	if err != nil {
		return "", err
	}

	return secret, nil
}

// GetSecrets retrieves multiple secrets from Bitwarden Secrets Manager using the provided list of secret IDs.
func (p *Provider) GetSecrets(ctx context.Context, uris []string) (map[string]string, error) {
	for _, uri := range uris {
		if err := onepassword.Secrets.ValidateSecretReference(ctx, uri); err != nil {
			return nil, err
		}
	}

	secrets, err := p.Client.Secrets().ResolveAll(ctx, uris)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string, len(secrets.IndividualResponses))
	for uri, secret := range secrets.IndividualResponses {
		if secret.Error != nil {
			return nil, fmt.Errorf("error resolving secret: %s", secret.Error)
		}

		result[uri] = secret.Content.Secret
	}

	return result, nil
}

// Close cleans up resources used by the Provider.
func (p *Provider) Close() {
	// No resources to clean up for 1Password client
}
