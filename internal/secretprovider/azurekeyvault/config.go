package azurekeyvault

import (
	"fmt"
	"os"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	VaultURL         config.HttpUrl `env:"SECRET_PROVIDER_VAULT_URL,notEmpty" validate:"httpUrl"`
	TenantID         string         `env:"AZURE_TENANT_ID"`
	ClientID         string         `env:"AZURE_CLIENT_ID"`
	ClientSecret     string         `env:"AZURE_CLIENT_SECRET"`           // #nosec G117 -- Azure service principal client secret
	ClientSecretFile string         `env:"AZURE_CLIENT_SECRET_FILE,file"` // Path to a file containing the Azure service principal client secret

	clientSecretFromFile bool
}

// GetConfig retrieves and validates the Azure Key Vault configuration.
func GetConfig() (*Config, error) {
	clientSecretFromFile := os.Getenv("AZURE_CLIENT_SECRET_FILE") != ""
	if clientSecretFromFile && os.Getenv("AZURE_CLIENT_SECRET") != "" {
		return nil, fmt.Errorf(
			"%w: %w: AZURE_CLIENT_SECRET or AZURE_CLIENT_SECRET_FILE",
			config.ErrParseConfigFailed,
			config.ErrBothSecretsSet,
		)
	}

	cfg := Config{
		clientSecretFromFile: clientSecretFromFile,
	}

	mappings := []config.EnvVarFileMapping{
		{
			EnvName:    "AZURE_CLIENT_SECRET",
			EnvValue:   &cfg.ClientSecret,
			FileValue:  &cfg.ClientSecretFile,
			AllowUnset: true,
		},
	}

	if err := config.ParseConfigFromEnv(&cfg, &mappings); err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	if cfg.clientSecretFromFile && cfg.ClientSecret == "" {
		return nil, fmt.Errorf("%w: AZURE_CLIENT_SECRET_FILE must not be empty", config.ErrParseConfigFailed)
	}

	return &cfg, nil
}
