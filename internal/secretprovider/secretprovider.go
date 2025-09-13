package secretprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/compose-spec/compose-go/v2/types"

	onepassword "github.com/kimdre/doco-cd/internal/secretprovider/1password"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
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
	ResolveSecretReferences(ctx context.Context, secrets map[string]string) (map[string]string, error)
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

// InjectSecretsToProject resolves and injects external secrets into the environment variables of the given project.
func InjectSecretsToProject(ctx context.Context, provider *SecretProvider, project *types.Project, secrets map[string]string) error {
	// If no provider is set or no secrets are provided we skip the secret injection
	if len(secrets) == 0 {
		return nil
	}

	if provider == nil || *provider == nil {
		return errors.New("no secret provider configured, but secrets are defined")
	}

	// Resolve external secrets
	resolvedSecrets, err := (*provider).ResolveSecretReferences(ctx, secrets)
	if err != nil {
		return fmt.Errorf("failed to resolve secrets: %w", err)
	}

	// Inject resolved secrets into each service's environment
	for i, service := range project.Services {
		if service.Environment == nil {
			service.Environment = types.MappingWithEquals{}
		}

		for envVar, secretValue := range resolvedSecrets {
			service.Environment[envVar] = &secretValue
		}

		project.Services[i] = service
	}

	return nil
}
