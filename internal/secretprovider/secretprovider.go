package secretprovider

import (
	"errors"
	"fmt"

	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
)

// SecretProvider defines the interface for secret providers.
type SecretProvider interface {
	Name() string
	GetSecret(id string) (string, error)
	GetSecrets(ids []string) (map[string]string, error)
	Close()
}

var ErrUnknownProvider = errors.New("unknown secret provider")

// Initialize initializes the secret provider based on the provided configuration.
func Initialize(provider string) (SecretProvider, error) {
	switch provider {
	case bitwardensecretsmanager.Name:
		cfg, err := bitwardensecretsmanager.GetConfig()
		if err != nil {
			return nil, err
		}

		return bitwardensecretsmanager.NewProvider(cfg.ApiUrl, cfg.IdentityUrl, cfg.AccessToken)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, provider)
	}
}
