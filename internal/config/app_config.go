package config

import (
	"github.com/caarlos0/env/v11"
)

// AppConfig is used to configure this application
type AppConfig struct {
	LogLevel       string `env:"LOG_LEVEL,required" envDefault:"info"` // LogLevel is the log level for the application
	HttpPort       uint16 `env:"HTTP_PORT,required" envDefault:"80"`   // HttpPort is the port the HTTP server will listen on
	WebhookSecret  string `env:"WEBHOOK_SECRET,required"`              // WebhookSecret is the secret used to authenticate the webhook
	GitAccessToken string `env:"GIT_ACCESS_TOKEN"`                     // GitAccessToken is the access token used to authenticate with the Git server (e.g. GitHub) for private repositories
}

// GetAppConfig returns the configuration
func GetAppConfig() (*AppConfig, error) {
	cfg := AppConfig{}
	if err := env.Parse(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
