package secretprovider

import (
	"context"
	"errors"
	"fmt"

	onepassword "github.com/kimdre/doco-cd/internal/secretprovider/1password"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
)

// SecretProvider defines the interface for secret providers.
type SecretProvider interface {
	Name() string
	GetSecret(ctx context.Context, id string) (string, error)
	GetSecrets(ctx context.Context, ids []string) (map[string]string, error)
	Close()
}

var ErrUnknownProvider = errors.New("unknown secret provider")

// Initialize initializes the secret provider based on the provided configuration.
func Initialize(ctx context.Context, provider, version string) (SecretProvider, error) {
	switch provider {
	case bitwardensecretsmanager.Name:
		cfg, err := bitwardensecretsmanager.GetConfig()
		if err != nil {
			return nil, err
		}

		return bitwardensecretsmanager.NewProvider(cfg.ApiUrl, cfg.IdentityUrl, cfg.AccessToken)
	case onepassword.Name:
		cfg, err := onepassword.GetConfig()
		if err != nil {
			return nil, err
		}

		return onepassword.NewProvider(ctx, cfg.AccessToken, version)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, provider)
	}
}
