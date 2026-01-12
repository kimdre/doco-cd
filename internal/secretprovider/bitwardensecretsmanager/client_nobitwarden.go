//go:build nobitwarden

package bitwardensecretsmanager

import (
	"context"
	"errors"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const (
	Name = "bitwarden_sm"
)

var ErrNotSupported = errors.New("bitwarden secrets manager is not supported in this build")

type Provider struct{}

// Name returns the name of the secret provider.
func (p *Provider) Name() string {
	return Name
}

// NewProvider returns an error indicating Bitwarden is not supported in this build.
func NewProvider(apiUrl, identityURL, accessToken string) (*Provider, error) {
	return nil, ErrNotSupported
}

// GetSecret returns an error indicating Bitwarden is not supported in this build.
func (p *Provider) GetSecret(_ context.Context, id string) (string, error) {
	return "", ErrNotSupported
}

// GetSecrets returns an error indicating Bitwarden is not supported in this build.
func (p *Provider) GetSecrets(_ context.Context, ids []string) (map[string]string, error) {
	return nil, ErrNotSupported
}

// ResolveSecretReferences returns an error indicating Bitwarden is not supported in this build.
func (p *Provider) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
	return nil, ErrNotSupported
}

// Close is a no-op in the no-bitwarden build.
func (p *Provider) Close() {}
