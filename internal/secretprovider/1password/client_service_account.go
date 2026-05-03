package onepassword

import (
	"context"
	"fmt"
	"strings"

	opsdk "github.com/1password/onepassword-sdk-go"
)

type serviceAccountClient = opsdk.Client

func (p *Provider) initializeServiceAccountClient(ctx context.Context) error {
	client, err := opsdk.NewClient(
		ctx,
		opsdk.WithServiceAccountToken(p.accessToken),
		opsdk.WithIntegrationInfo("doco-cd", p.version),
	)
	if err != nil {
		return err
	}

	p.serviceClient = client

	return nil
}

func (p *Provider) renewServiceAccountSession(ctx context.Context) error {
	if err := p.initializeServiceAccountClient(ctx); err != nil {
		return fmt.Errorf("failed to renew secret provider client session: %w", err)
	}

	return nil
}

func (p *Provider) resolveServiceAccountSecret(ctx context.Context, uri string) (string, error) {
	if err := opsdk.Secrets.ValidateSecretReference(ctx, uri); err != nil {
		return "", err
	}

	secret, err := p.serviceClient.Secrets().Resolve(ctx, uri)
	if err != nil {
		if strings.Contains(err.Error(), ErrInvalidClientID.Error()) {
			if renewErr := p.renewServiceAccountSession(ctx); renewErr != nil {
				return "", renewErr
			}

			secret, err = p.serviceClient.Secrets().Resolve(ctx, uri)
			if err != nil {
				return "", fmt.Errorf("failed to resolve secret after renewing session: %w", err)
			}
		} else {
			return "", err
		}
	}

	return secret, nil
}

func (p *Provider) resolveServiceAccountSecrets(ctx context.Context, uris []string) (map[string]string, error) {
	for _, uri := range uris {
		if err := opsdk.Secrets.ValidateSecretReference(ctx, uri); err != nil {
			return nil, err
		}
	}

	secrets, err := p.serviceClient.Secrets().ResolveAll(ctx, uris)
	if err != nil {
		if strings.Contains(err.Error(), ErrInvalidClientID.Error()) {
			if renewErr := p.renewServiceAccountSession(ctx); renewErr != nil {
				return nil, renewErr
			}

			secrets, err = p.serviceClient.Secrets().ResolveAll(ctx, uris)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve secrets after renewing session: %w", err)
			}
		} else {
			return nil, err
		}
	}

	result := make(map[string]string, len(uris))

	for uri, secret := range secrets.IndividualResponses {
		if secret.Error != nil {
			return nil, fmt.Errorf("error resolving secret '%s': %s", uri, secret.Error.Type)
		}

		result[uri] = secret.Content.Secret
	}

	return result, nil
}
