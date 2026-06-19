package onepassword

import (
	"context"
	"fmt"
)

func (p *Provider) resolveSecret(ctx context.Context, uri string) (string, error) {
	switch p.mode {
	case authModeConnect:
		return p.resolveConnectSecret(ctx, uri)
	case authModeServiceAccount:
		return p.resolveServiceAccountSecret(ctx, uri)
	default:
		return "", fmt.Errorf("unsupported 1password auth mode: %s", p.mode)
	}
}

func (p *Provider) resolveSecrets(ctx context.Context, uris []string) (map[string]string, error) {
	switch p.mode {
	case authModeConnect:
		return p.resolveConnectSecrets(ctx, uris)
	case authModeServiceAccount:
		return p.resolveServiceAccountSecrets(ctx, uris)
	default:
		return nil, fmt.Errorf("unsupported 1password auth mode: %s", p.mode)
	}
}
