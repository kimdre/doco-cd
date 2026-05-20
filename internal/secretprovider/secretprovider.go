package secretprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/kimdre/doco-cd/internal/secretprovider/grpc"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
	"github.com/kimdre/doco-cd/internal/secretprovider/webhook"
)

// SecretValueProvider describes an implementation capable of retrieving secret values.
type SecretValueProvider interface {
	// GetSecret retrieves a secret value from the secret provider using the provided secret ID.
	GetSecret(ctx context.Context, id string) (string, error)
}

// SecretProvider defines the interface for secret providers.
type SecretProvider interface {
	SecretValueProvider

	// Name returns the name of the secret provider.
	Name() string
	// GetSecrets retrieves multiple secrets from the secret provider using the provided list of secret IDs.
	GetSecrets(ctx context.Context, ids []string) (map[string]string, error)
	// ResolveSecretReferences resolves the provided map of environment variable names to secret IDs
	// by fetching the corresponding secret values from the secret provider.
	ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error)
	// Close cleans up resources used by the Provider.
	Close()
}

// The SecretValueProviderFunc type is an adapter to allow the use of ordinary
// functions as secret providers. If f is a function with the appropriate signature,
// SecretValueProviderFunc(f) is a SecretValueProvider that calls f.
type SecretValueProviderFunc func(ctx context.Context, id string) (string, error)

// GetSecret calls f(ctx, id).
func (f SecretValueProviderFunc) GetSecret(ctx context.Context, id string) (string, error) {
	return f(ctx, id)
}

var ErrUnknownProvider = errors.New("unknown secret provider")

// Initialize initializes the secret provider based on the provided configuration.
// The returned provider is wrapped with retry logic to handle transient
// rate-limit errors (HTTP 429) from upstream APIs.
func Initialize(ctx context.Context, provider, _ string) (SecretProvider, error) {
	switch provider {
	case "":
		return nil, nil

	case grpc.Name:
		cfg, err := grpc.GetConfig()
		if err != nil {
			return nil, err
		}

		prov, err := grpc.NewValueProvider(ctx, cfg)
		if err != nil {
			return nil, err
		}

		return NewRetryingSecretProvider(AdaptSecretValueProvider(grpc.Name, prov)), nil

	case webhook.Name:
		cfg, err := webhook.GetConfig()
		if err != nil {
			return nil, err
		}

		prov, err := webhook.NewValueProvider(ctx, cfg)
		if err != nil {
			return nil, err
		}

		return NewRetryingSecretProvider(AdaptSecretValueProvider(webhook.Name, prov)), nil

	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, provider)
	}
}
