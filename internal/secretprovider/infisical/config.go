package infisical

import (
	"fmt"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	SiteUrl          string `env:"SECRET_PROVIDER_SITE_URL,notEmpty" envDefault:"https://app.infisical.com"`
	ClientID         string `env:"SECRET_PROVIDER_CLIENT_ID,notEmpty"`               // Client ID for authenticating with the secret provider
	ClientSecret     string `env:"SECRET_PROVIDER_CLIENT_SECRET" validate:"nonzero"` // #nosec G117 -- Client secret for authenticating with the secret provider
	ClientSecretFile string `env:"SECRET_PROVIDER_CLIENT_SECRET_FILE,file"`          // Path to a file containing the client secret
}

// GetConfig retrieves and parses the configuration for the Bitwarden Secrets Manager from environment variables.
func GetConfig() (*Config, error) {
	cfg := Config{}

	mappings := []config.EnvVarFileMapping{
		{EnvName: "SECRET_PROVIDER_CLIENT_SECRET", EnvValue: &cfg.ClientSecret, FileValue: &cfg.ClientSecretFile, AllowUnset: false},
	}

	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	return &cfg, nil
}
