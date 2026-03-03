package secretprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	onepassword "github.com/kimdre/doco-cd/internal/secretprovider/1password"
	"github.com/kimdre/doco-cd/internal/secretprovider/awssecretsmanager"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
	"github.com/kimdre/doco-cd/internal/secretprovider/infisical"
	"github.com/kimdre/doco-cd/internal/secretprovider/openbao"
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
func Initialize(ctx context.Context, provider, version string) (SecretProvider, error) {
	if provider == "" {
		return nil, nil
	}

	var (
		p   SecretProvider
		err error
	)

	switch provider {
	case awssecretsmanager.Name:
		cfg, cfgErr := awssecretsmanager.GetConfig()
		if cfgErr != nil {
			return nil, cfgErr
		}

		p, err = awssecretsmanager.NewProvider(ctx, cfg.Region, cfg.AccessKeyID, cfg.SecretAccessKey)
	case bitwardensecretsmanager.Name:
		cfg, cfgErr := bitwardensecretsmanager.GetConfig()
		if cfgErr != nil {
			return nil, cfgErr
		}

		p, err = bitwardensecretsmanager.NewProvider(cfg.ApiUrl, cfg.IdentityUrl, cfg.AccessToken)
	case onepassword.Name:
		cfg, cfgErr := onepassword.GetConfig()
		if cfgErr != nil {
			return nil, cfgErr
		}

		p, err = onepassword.NewProvider(ctx, cfg.AccessToken, version)
	case infisical.Name:
		cfg, cfgErr := infisical.GetConfig()
		if cfgErr != nil {
			return nil, cfgErr
		}

		p, err = infisical.NewProvider(ctx, cfg.SiteUrl, cfg.ClientID, cfg.ClientSecret)
	case openbao.Name:
		cfg, cfgErr := openbao.GetConfig()
		if cfgErr != nil {
			return nil, cfgErr
		}

		p, err = openbao.NewProvider(ctx, cfg.SiteUrl, cfg.AccessToken)
	case webhook.Name:
		cfg, cfgErr := webhook.GetConfig()
		if cfgErr != nil {
			return nil, cfgErr
		}

		prov, provErr := webhook.NewValueProvider(ctx, cfg)
		if provErr != nil {
			return nil, provErr
		}

		p = AdaptSecretValueProvider(webhook.Name, prov)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, provider)
	}

	if err != nil {
		return nil, err
	}

	return NewRetryingSecretProvider(p), nil
}

// Hash returns a SHA256 hash of the ExternalSecrets map.
func Hash(secrets secrettypes.ResolvedSecrets) string {
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		_, _ = sb.WriteString(k)
		_, _ = sb.WriteString("=")
		_, _ = sb.WriteString(secrets[k])
		_, _ = sb.WriteString(";")
	}

	sum := sha256.Sum256([]byte(sb.String()))

	return hex.EncodeToString(sum[:])
}
