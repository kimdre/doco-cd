package config

import (
	"github.com/caarlos0/env/v11"
)

// AppConfig is used to configure this application
type AppConfig struct {
	HttpPort       string `env:"HTTP_PORT" envDefault:"80"` // HttpPort is the port the HTTP server will listen on
	WebhookSecret  string `env:"WEBHOOK_SECRET"`            // WebhookSecret is the secret used to authenticate the webhook
	GitUsername    string `env:"GIT_USERNAME"`              // GitUsername is the username used to authenticate with the git server
	GitAccessToken string `env:"GIT_ACCESS_TOKEN"`          // GitAccessToken is the access token used to authenticate with the git server
}

// GetAppConfig returns the configuration
func GetAppConfig() (*AppConfig, error) {
	cfg := AppConfig{}
	if err := env.Parse(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
