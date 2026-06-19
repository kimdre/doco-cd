package awssecretsmanager

import (
	"fmt"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	Region              string `env:"SECRET_PROVIDER_REGION,notEmpty"`
	AccessKeyID         string `env:"SECRET_PROVIDER_ACCESS_KEY_ID,notEmpty"`
	SecretAccessKey     string `env:"SECRET_PROVIDER_SECRET_ACCESS_KEY" validate:"nonzero"`
	SecretAccessKeyFile string `env:"SECRET_PROVIDER_SECRET_ACCESS_KEY_FILE,file"`
}

// GetConfig retrieves and parses the configuration for the Bitwarden Secrets Manager from environment variables.
func GetConfig() (*Config, error) {
	cfg := Config{}

	mappings := []config.EnvVarFileMapping{
		{EnvName: "SECRET_PROVIDER_SECRET_ACCESS_KEY", EnvValue: &cfg.SecretAccessKey, FileValue: &cfg.SecretAccessKeyFile, AllowUnset: false},
	}

	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	return &cfg, nil
}
