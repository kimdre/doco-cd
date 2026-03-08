package bitwardenvault

import (
	"fmt"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	ApiUrl                 string `env:"SECRET_PROVIDER_API_URL,notEmpty" envDefault:"http://localhost:8080/api"`
	AccessToken            string `env:"SECRET_PROVIDER_ACCESS_TOKEN"`
	AccessTokenFile        string `env:"SECRET_PROVIDER_ACCESS_TOKEN_FILE,file"`
	OAuth2ClientID         string `env:"SECRET_PROVIDER_OAUTH2_CLIENT_ID"`
	OAuth2ClientSecret     string `env:"SECRET_PROVIDER_OAUTH2_CLIENT_SECRET"`
	OAuth2ClientSecretFile string `env:"SECRET_PROVIDER_OAUTH2_CLIENT_SECRET_FILE,file"`
	OAuth2TokenURL         string `env:"SECRET_PROVIDER_OAUTH2_TOKEN_URL"`
}

func GetConfig() (*Config, error) {
	cfg := Config{}
	mappings := []config.EnvVarFileMapping{
		// Only require ACCESS_TOKEN if OAuth2 is not configured
		{EnvName: "SECRET_PROVIDER_ACCESS_TOKEN", EnvValue: &cfg.AccessToken, FileValue: &cfg.AccessTokenFile, AllowUnset: true},
		{EnvName: "SECRET_PROVIDER_OAUTH2_CLIENT_SECRET", EnvValue: &cfg.OAuth2ClientSecret, FileValue: &cfg.OAuth2ClientSecretFile, AllowUnset: true},
	}
	_ = config.ParseConfigFromEnv(&cfg, &[]config.EnvVarFileMapping{}) // Preload OAuth2 fields for conditional logic
	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	return &cfg, nil
}
