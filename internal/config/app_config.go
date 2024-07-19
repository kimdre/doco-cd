package config

import (
	"github.com/caarlos0/env/v11"
)

// AppConfig is used to configure this application
type AppConfig struct {
	HttpPort            string `env:"HTTP_PORT" envDefault:"80"` // HttpPort is the port the HTTP server will listen on
	GithubWebhookSecret string `env:"GITHUB_WEBHOOK_SECRET"`     // GithubWebhookSecret is the secret used to authenticate the webhook
	GitUsername         string `env:"GITHUB_USERNAME"`           // GitUsername is the username used to authenticate to Github
	GitPassword         string `env:"Github_PASSWORD"`           // GitPassword is the password or token used to authenticate to Github
}

// GetAppConfig returns the configuration
func GetAppConfig() (*AppConfig, error) {
	cfg := AppConfig{}
	if err := env.Parse(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
