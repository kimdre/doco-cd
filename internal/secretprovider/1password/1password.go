package onepassword

import (
	"context"
	"fmt"

	"github.com/1password/onepassword-sdk-go"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
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
			return nil, fmt.Errorf("error resolving secret '%s': %s", uri, secret.Error.Type)
		}

		result[uri] = secret.Content.Secret
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
	// No resources to clean up for 1Password client
}
