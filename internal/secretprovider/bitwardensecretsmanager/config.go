package bitwardensecretsmanager

import (
	"fmt"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

type Config struct {
	*secretprovider.BaseConfig
	ApiUrl          string `env:"SECRET_PROVIDER_API_URL,notEmpty" envDefault:"https://vault.bitwarden.com/api"`
	IdentityUrl     string `env:"SECRET_PROVIDER_IDENTITY_URL,notEmpty" envDefault:"https://vault.bitwarden.com/identity"`
	AccessToken     string `env:"SECRET_PROVIDER_ACCESS_TOKEN" validate:"nonzero"` // Access token for authenticating with the secret provider
	AccessTokenFile string `env:"SECRET_PROVIDER_ACCESS_TOKEN_FILE,file"`          // Path to a file containing the access token
}

func GetConfig() (*Config, error) {
	cfg := Config{}

	mappings := []config.EnvVarFileMapping{
		{EnvName: "SECRET_PROVIDER_ACCESS_TOKEN", EnvValue: &cfg.AccessToken, FileValue: &cfg.AccessTokenFile, AllowUnset: true},
	}

	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config from environment: %w", err)
	}

	return &cfg, nil
}
