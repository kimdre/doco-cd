package bitwardenvault

import (
	"errors"
	"fmt"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	ApiUrl                 string `env:"SECRET_PROVIDER_API_URL,notEmpty" envDefault:"https://api.bitwarden.com"` // For self-hosted, e.g. https://vault.example.com/api
	OAuth2ClientID         string `env:"SECRET_PROVIDER_OAUTH2_CLIENT_ID"`
	OAuth2ClientSecret     string `env:"SECRET_PROVIDER_OAUTH2_CLIENT_SECRET"`
	OAuth2ClientSecretFile string `env:"SECRET_PROVIDER_OAUTH2_CLIENT_SECRET_FILE,file"`
	OAuth2TokenURL         string `env:"SECRET_PROVIDER_OAUTH2_TOKEN_URL" envDefault:"https://identity.bitwarden.com/connect/token"` // For self-hosted, e.g. https://vault.example.com/identity/connect/token
	AppDataDir             string `env:"SECRET_PROVIDER_APPDATA_DIR" envDefault:"/data/.config/bitwarden-cli"`                       // Data directory for bw CLI to store config/session
}

func GetConfig() (*Config, error) {
	cfg := Config{}
	mappings := []config.EnvVarFileMapping{
		{EnvName: "SECRET_PROVIDER_OAUTH2_CLIENT_SECRET", EnvValue: &cfg.OAuth2ClientSecret, FileValue: &cfg.OAuth2ClientSecretFile, AllowUnset: true},
	}

	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	if cfg.OAuth2ClientID == "" || cfg.OAuth2ClientSecret == "" || cfg.OAuth2TokenURL == "" || cfg.ApiUrl == "" {
		return nil, errors.New("OAuth2 configuration is required for bitwarden_vault provider")
	}

	return &cfg, nil
}
