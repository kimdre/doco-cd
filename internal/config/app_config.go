package config

import (
	"errors"
	"strings"

	"github.com/caarlos0/env/v11"
	"gopkg.in/validator.v2"
)

// AppConfig is used to configure this application
// https://github.com/caarlos0/env?tab=readme-ov-file#env-tag-options
type AppConfig struct {
	LogLevel            string `env:"LOG_LEVEL,required" envDefault:"info"`                          // LogLevel is the log level for the application
	HttpPort            uint16 `env:"HTTP_PORT,required" envDefault:"80" validate:"min=1,max=65535"` // HttpPort is the port the HTTP server will listen on
	WebhookSecret       string `env:"WEBHOOK_SECRET,required,notEmpty"`                              // WebhookSecret is the secret used to authenticate the webhook
	GitAccessToken      string `env:"GIT_ACCESS_TOKEN"`                                              // GitAccessToken is the access token used to authenticate with the Git server (e.g. GitHub) for private repositories
	AuthType            string `env:"AUTH_TYPE" envDefault:"oauth2"`                                 // AuthType is the type of authentication to use when cloning repositories
	SkipTLSVerification bool   `env:"SKIP_TLS_VERIFICATION" envDefault:"false"`                      // SkipTLSVerification skips the TLS verification when cloning repositories.
	DockerQuietDeploy   bool   `env:"DOCKER_QUIET_DEPLOY" envDefault:"true"`                         // DockerQuietDeploy suppresses the status output of dockerCli in deployments (e.g. pull, create, start)
}

const DockerSecretsPath = "/run/secrets"

var ErrInvalidLogLevel = validator.TextErr{Err: errors.New("invalid log level, must be one of debug, info, warn, error")}

// GetAppConfig returns the configuration
func GetAppConfig(secretsPath string) (*AppConfig, error) {
	cfg := AppConfig{}

	// Load env vars from Docker secrets if used
	if err := loadEnvFromDockerSecrets(secretsPath); err != nil {
		return nil, err
	}

	// Parse app config from environment variables
	if err := env.Parse(&cfg); err != nil {
		return nil, err
	}

	logLvl := strings.ToLower(cfg.LogLevel)
	if logLvl != "debug" && logLvl != "info" && logLvl != "warn" && logLvl != "error" {
		return nil, ErrInvalidLogLevel
	}

	if err := validator.Validate(cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
