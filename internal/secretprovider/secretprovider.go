package secretprovider

import (
	"context"
	"errors"
	"fmt"

	onepassword "github.com/kimdre/doco-cd/internal/secretprovider/1password"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// SecretProvider defines the interface for secret providers.
type SecretProvider interface {
	// Name returns the name of the secret provider.
	Name() string
	// GetSecret retrieves a secret value from the secret provider using the provided secret ID.
	GetSecret(ctx context.Context, id string) (string, error)
	// GetSecrets retrieves multiple secrets from the secret provider using the provided list of secret IDs.
	GetSecrets(ctx context.Context, ids []string) (map[string]string, error)
	// ResolveSecretReferences resolves the provided map of environment variable names to secret IDs
	// by fetching the corresponding secret values from the secret provider.
	ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error)
	// Close cleans up resources used by the Provider.
	Close()
}

var ErrUnknownProvider = errors.New("unknown secret provider")

// Initialize initializes the secret provider based on the provided configuration.
func Initialize(ctx context.Context, provider, version string) (SecretProvider, error) {
	if provider == "" {
		return nil, nil
	}

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
