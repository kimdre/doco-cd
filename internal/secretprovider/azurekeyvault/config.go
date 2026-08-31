package azurekeyvault

import (
	"fmt"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	VaultURL config.HttpUrl `env:"SECRET_PROVIDER_VAULT_URL,notEmpty" validate:"httpUrl"`
}

// GetConfig retrieves and validates the Azure Key Vault configuration.
func GetConfig() (*Config, error) {
	cfg := Config{}

	var mappings []config.EnvVarFileMapping

	if err := config.ParseConfigFromEnv(&cfg, &mappings); err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	return &cfg, nil
}
