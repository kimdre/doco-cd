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
	case awssecretsmanager.Name:
		cfg, err := awssecretsmanager.GetConfig()
		if err != nil {
			return nil, err
		}

		return awssecretsmanager.NewProvider(ctx, cfg.Region, cfg.AccessKeyID, cfg.SecretAccessKey)
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
	case infisical.Name:
		cfg, err := infisical.GetConfig()
		if err != nil {
			return nil, err
		}

		return infisical.NewProvider(ctx, cfg.SiteUrl, cfg.ClientID, cfg.ClientSecret)
	case openbao.Name:
		cfg, err := openbao.GetConfig()
		if err != nil {
			return nil, err
		}

		return openbao.NewProvider(ctx, cfg.SiteUrl, cfg.AccessToken)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, provider)
	}
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
