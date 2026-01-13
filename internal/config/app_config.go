package config

import (
	"fmt"
	"strings"

	"github.com/kimdre/doco-cd/internal/notification"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"go.yaml.in/yaml/v3"
	"gopkg.in/validator.v2"
)

const AppName = "doco-cd" // Name of the application

var AppVersion = "dev" // Version of the application, to be set during build time

// AppConfig is used to configure this application
// https://github.com/caarlos0/env?tab=readme-ov-file#env-tag-options
type AppConfig struct {
	LogLevel                    string                 `env:"LOG_LEVEL,notEmpty" envDefault:"info"`                          // LogLevel is the log level for the application
	HttpPort                    uint16                 `env:"HTTP_PORT,notEmpty" envDefault:"80" validate:"min=1,max=65535"` // HttpPort is the port the HTTP server will listen on
	HttpProxyString             string                 `env:"HTTP_PROXY"`                                                    // HttpProxyString is the HTTP proxy URL as a string
	HttpProxy                   transport.ProxyOptions // HttpProxy is the HTTP proxy configuration parsed from the HttpProxyString
	ApiSecret                   string                 `env:"API_SECRET"`                                                         // ApiSecret is the secret token used to authenticate with the API
	ApiSecretFile               string                 `env:"API_SECRET_FILE,file"`                                               // ApiSecretFile is the file containing the ApiSecret
	WebhookSecret               string                 `env:"WEBHOOK_SECRET"`                                                     // WebhookSecret is the secret token used to authenticate the webhook
	WebhookSecretFile           string                 `env:"WEBHOOK_SECRET_FILE,file"`                                           // WebhookSecretFile is the file containing the WebhookSecret
	GitAccessToken              string                 `env:"GIT_ACCESS_TOKEN"`                                                   // GitAccessToken is the access token used to authenticate with the Git server (e.g. GitHub) for private repositories
	GitAccessTokenFile          string                 `env:"GIT_ACCESS_TOKEN_FILE,file"`                                         // GitAccessTokenFile is the file containing the GitAccessToken
	GitCloneSubmodules          bool                   `env:"GIT_CLONE_SUBMODULES,notEmpty" envDefault:"true"`                    // GitCloneSubmodules controls whether git submodules are cloned
	SSHPrivateKey               string                 `env:"SSH_PRIVATE_KEY"`                                                    // SSHPrivateKey is the SSH private key used for SSH authentication with Git repositories
	SSHPrivateKeyFile           string                 `env:"SSH_PRIVATE_KEY_FILE,file"`                                          // SSHPrivateKeyFile is the file containing the SSHPrivateKey
	SSHPrivateKeyPassphrase     string                 `env:"SSH_PRIVATE_KEY_PASSPHRASE"`                                         // SSHPrivateKeyPassphrase is the passphrase for the SSH private key, if applicable
	SSHPrivateKeyPassphraseFile string                 `env:"SSH_PRIVATE_KEY_PASSPHRASE_FILE,file"`                               // SSHPrivateKeyPassphraseFile is the file containing the SSHPrivateKeyPassphrase
	AuthType                    string                 `env:"AUTH_TYPE,notEmpty" envDefault:"oauth2"`                             // AuthType is the type of authentication to use when cloning repositories
	SkipTLSVerification         bool                   `env:"SKIP_TLS_VERIFICATION,notEmpty" envDefault:"false"`                  // SkipTLSVerification skips the TLS verification when cloning repositories.
	DockerQuietDeploy           bool                   `env:"DOCKER_QUIET_DEPLOY,notEmpty" envDefault:"true"`                     // DockerQuietDeploy suppresses the status output of dockerCli in deployments (e.g. pull, create, start)
	DockerSwarmFeatures         bool                   `env:"DOCKER_SWARM_FEATURES,notEmpty" envDefault:"true"`                   // DockerSwarmFeatures enables the usage Docker Swarm features in the application if it has detected that it is running in a Docker Swarm environment
	DeployConfigBaseDir         string                 `env:"DEPLOY_CONFIG_BASE_DIR" envDefault:"/"`                              // DeployConfigBaseDir is the base directory (relative to the repository root) where deployment configuration files will be searched for.
	PollConfigYAML              string                 `env:"POLL_CONFIG"`                                                        // PollConfigYAML is the unparsed string containing the PollConfig in YAML format
	PollConfigFile              string                 `env:"POLL_CONFIG_FILE,file"`                                              // PollConfigFile is the file containing the PollConfig in YAML format
	PollConfig                  []PollConfig           `yaml:"-"`                                                                 // PollConfig is the YAML configuration for polling Git repositories for changes
	MaxPayloadSize              int64                  `env:"MAX_PAYLOAD_SIZE,notEmpty" envDefault:"1048576"`                     // MaxPayloadSize is the maximum size of the payload in bytes that the HTTP server will accept (default 1MB = 1048576 bytes)
	MetricsPort                 uint16                 `env:"METRICS_PORT,notEmpty" envDefault:"9120" validate:"min=1,max=65535"` // MetricsPort is the port the prometheus metrics server will listen on
	AppriseApiURL               HttpUrl                `env:"APPRISE_API_URL" validate:"httpUrl"`                                 // AppriseApiURL is the URL of the Apprise notification service
	AppriseNotifyUrls           string                 `env:"APPRISE_NOTIFY_URLS"`                                                // AppriseNotifyUrls is a comma-separated list of URLs to notify via the Apprise notification service
	AppriseNotifyUrlsFile       string                 `env:"APPRISE_NOTIFY_URLS_FILE,file"`                                      // AppriseNotifyUrlsFile is the file containing the AppriseNotifyUrls
	AppriseNotifyLevel          string                 `env:"APPRISE_NOTIFY_LEVEL,notEmpty" envDefault:"success"`                 // AppriseNotifyLevel is the level of notifications to send via the Apprise notification service
	SecretProvider              string                 `env:"SECRET_PROVIDER"`                                                    // SecretProvider is the secret provider/manager to use for retrieving secrets (e.g. bitwarden secrets manager)
	MaxDeploymentLoopCount      uint                   `env:"MAX_DEPLOYMENT_LOOP_COUNT,notEmpty" envDefault:"2" validate:"min=0"` // Maximum allowed deployment loops before a forced deployment is triggered
}

// GetAppConfig returns the configuration.
func GetAppConfig() (*AppConfig, error) {
	cfg := AppConfig{}

	mappings := []EnvVarFileMapping{
		{EnvName: "API_SECRET", EnvValue: &cfg.ApiSecret, FileValue: &cfg.ApiSecretFile, AllowUnset: true},
		{EnvName: "APPRISE_NOTIFY_URLS", EnvValue: &cfg.AppriseNotifyUrls, FileValue: &cfg.AppriseNotifyUrlsFile, AllowUnset: true},
		{EnvName: "GIT_ACCESS_TOKEN", EnvValue: &cfg.GitAccessToken, FileValue: &cfg.GitAccessTokenFile, AllowUnset: true},
		{EnvName: "SSH_PRIVATE_KEY", EnvValue: &cfg.SSHPrivateKey, FileValue: &cfg.SSHPrivateKeyFile, AllowUnset: true},
		{EnvName: "SSH_PRIVATE_KEY_PASSPHRASE", EnvValue: &cfg.SSHPrivateKeyPassphrase, FileValue: &cfg.SSHPrivateKeyPassphraseFile, AllowUnset: true},
		{EnvName: "WEBHOOK_SECRET", EnvValue: &cfg.WebhookSecret, FileValue: &cfg.WebhookSecretFile, AllowUnset: true},
	}

	err := ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrParseConfigFailed, err)
	}

	err = cfg.ParsePollConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to parse poll config: %w", err)
	}

	for _, pollConfig := range cfg.PollConfig {
		if err = pollConfig.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidPollConfig, err)
		}
	}

	logLvl := strings.ToLower(cfg.LogLevel)
	if logLvl != "debug" && logLvl != "info" && logLvl != "warn" && logLvl != "error" {
		return nil, ErrInvalidLogLevel
	}

	if err = validator.Validate(cfg); err != nil {
		return nil, err
	}

	if cfg.HttpPort == cfg.MetricsPort {
		return nil, fmt.Errorf("HTTP_PORT and METRICS_PORT cannot be the same port number: %d", cfg.HttpPort)
	}

	if cfg.HttpProxyString != "" {
		cfg.HttpProxy = transport.ProxyOptions{
			URL: cfg.HttpProxyString,
		}

		err = cfg.HttpProxy.Validate()
		if err != nil {
			return nil, fmt.Errorf("failed to validate HTTP_PROXY: %w", err)
		}
	}

	notification.SetAppriseConfig(
		string(cfg.AppriseApiURL),
		cfg.AppriseNotifyUrls,
		cfg.AppriseNotifyLevel,
	)

	return &cfg, nil
}

// ParsePollConfig parses the PollConfig from either the PollConfigYAML string or the PollConfigFile.
func (cfg *AppConfig) ParsePollConfig() error {
	if cfg.PollConfigYAML != "" && cfg.PollConfigFile != "" {
		return ErrBothPollConfigSet
	}

	if cfg.PollConfigYAML != "" {
		return yaml.Unmarshal([]byte(cfg.PollConfigYAML), &cfg.PollConfig)
	}

	if cfg.PollConfigFile != "" {
		return yaml.Unmarshal([]byte(cfg.PollConfigFile), &cfg.PollConfig)
	}

	cfg.PollConfig = []PollConfig{} // Default to an empty slice if no config is provided

	return nil
}
