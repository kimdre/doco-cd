package config

import (
	"strings"

	"github.com/caarlos0/env/v11"
	"gopkg.in/validator.v2"
)

const AppName = "doco-cd" // Name of the application

// AppConfig is used to configure this application
// https://github.com/caarlos0/env?tab=readme-ov-file#env-tag-options
type AppConfig struct {
	LogLevel            string `env:"LOG_LEVEL,notEmpty" envDefault:"info"`                          // LogLevel is the log level for the application
	HttpPort            uint16 `env:"HTTP_PORT,notEmpty" envDefault:"80" validate:"min=1,max=65535"` // HttpPort is the port the HTTP server will listen on
	WebhookSecret       string `env:"WEBHOOK_SECRET"`                                                // WebhookSecret is the secret used to authenticate the webhook
	WebhookSecretFile   string `env:"WEBHOOK_SECRET_FILE,file"`                                      // WebhookSecretFile is the file containing the WebhookSecret
	GitAccessToken      string `env:"GIT_ACCESS_TOKEN"`                                              // GitAccessToken is the access token used to authenticate with the Git server (e.g. GitHub) for private repositories
	GitAccessTokenFile  string `env:"GIT_ACCESS_TOKEN_FILE,file"`                                    // GitAccessTokenFile is the file containing the GitAccessToken
	AuthType            string `env:"AUTH_TYPE,notEmpty" envDefault:"oauth2"`                        // AuthType is the type of authentication to use when cloning repositories
	SkipTLSVerification bool   `env:"SKIP_TLS_VERIFICATION,notEmpty" envDefault:"false"`             // SkipTLSVerification skips the TLS verification when cloning repositories.
	DockerQuietDeploy   bool   `env:"DOCKER_QUIET_DEPLOY,notEmpty" envDefault:"true"`                // DockerQuietDeploy suppresses the status output of dockerCli in deployments (e.g. pull, create, start)
}

// GetAppConfig returns the configuration
func GetAppConfig() (*AppConfig, error) {
	cfg := AppConfig{}

	// Parse app config from environment variables
	if err := env.Parse(&cfg); err != nil {
		return nil, err
	}

	// Load file-based environment variables
	if err := loadFileBasedEnvVars(&cfg); err != nil {
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
