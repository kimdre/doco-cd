package openbao

import (
	"fmt"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	SiteUrl         string `env:"SECRET_PROVIDER_SITE_URL,notEmpty"` // URL of the secret provider
	AccessToken     string `env:"SECRET_PROVIDER_ACCESS_TOKEN"`      // #nosec G117 -- Access token for authenticating with the secret provider
	AccessTokenFile string `env:"SECRET_PROVIDER_ACCESS_TOKEN_FILE"` // Path to a file containing the access token
}

// GetConfig retrieves and parses the configuration for the Secrets Manager from environment variables.
func GetConfig() (*Config, error) {
	cfg := Config{}

	var mappings []config.EnvVarFileMapping

	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	if cfg.AccessTokenFile != "" {
		if cfg.AccessToken != "" {
			return nil, fmt.Errorf("%w: %w: SECRET_PROVIDER_ACCESS_TOKEN or SECRET_PROVIDER_ACCESS_TOKEN_FILE", config.ErrParseConfigFailed, config.ErrBothSecretsSet)
		}

		cfg.AccessToken, err = newAccessTokenFile(cfg.AccessTokenFile).Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
		}
	} else if cfg.AccessToken == "" {
		return nil, fmt.Errorf("%w: %w: SECRET_PROVIDER_ACCESS_TOKEN or SECRET_PROVIDER_ACCESS_TOKEN_FILE", config.ErrParseConfigFailed, config.ErrBothSecretsNotSet)
	}

	return &cfg, nil
}
