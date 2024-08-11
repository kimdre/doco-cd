package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/caarlos0/env/v11"
	"gopkg.in/validator.v2"
)

// AppConfig is used to configure this application
type AppConfig struct {
	LogLevel            string `env:"LOG_LEVEL,required" envDefault:"info"`                                       // LogLevel is the log level for the application
	HttpPort            uint16 `env:"HTTP_PORT,required" envDefault:"80" validate:"min=1,max=65535"`              // HttpPort is the port the HTTP server will listen on
	WebhookSecret       string `env:"WEBHOOK_SECRET,required"`                                                    // WebhookSecret is the secret used to authenticate the webhook
	GitAccessToken      string `env:"GIT_ACCESS_TOKEN"`                                                           // GitAccessToken is the access token used to authenticate with the Git server (e.g. GitHub) for private repositories
	AuthType            string `env:"AUTH_TYPE" envDefault:"oauth2"`                                              // AuthType is the type of authentication to use when cloning repositories
	SkipTLSVerification bool   `env:"SKIP_TLS_VERIFICATION" envDefault:"false"`                                   // SkipTLSVerification skips the TLS verification when cloning repositories.
	DockerAPIVersion    string `env:"DOCKER_API_VERSION" envDefault:"v1.40" validate:"regexp=^v[0-9]+\\.[0-9]+$"` // DockerAPIVersion is the version of the Docker API to use
	DockerQuietDeploy   bool   `env:"DOCKER_QUIET_DEPLOY" envDefault:"true"`                                      // DockerQuietDeploy suppresses the status output of dockerCli in deployments (e.g. pull, create, start)
	SOPSKeyService      string `env:"SOPS_KEY_SERVICE"`                                                           // SOPSKeyService is the key service used for SOPS decryption (e.g., aws, gcp, azure, pgp)
	SOPSKeyID           string `env:"SOPS_KEY_ID"`                                                                // SOPSKeyID is the key ID used for SOPS decryption
	SOPSKeyFile         string `env:"SOPS_KEY_FILE"`                                                              // SOPSKeyFile is the path to the key file used for SOPS decryption (if applicable)
	SOPSKey             string `env:"SOPS_KEY_BASE64"`                                                            // SOPSKey is the base64-encoded key used for SOPS decryption (if applicable)
}

var (
	ErrInvalidLogLevel         = validator.TextErr{Err: errors.New("invalid log level, must be one of debug, info, warn, error")}
	ErrInvalidDockerAPIVersion = validator.TextErr{Err: errors.New("invalid Docker API version format, must be e.g. v1.40")}
)

// GetAppConfig returns the configuration
func GetAppConfig() (*AppConfig, error) {
	cfg := AppConfig{}
	if err := env.Parse(&cfg); err != nil {
		return nil, err
	}

	logLvl := strings.ToLower(cfg.LogLevel)
	if logLvl != "debug" && logLvl != "info" && logLvl != "warn" && logLvl != "error" {
		return nil, ErrInvalidLogLevel
	}

	if err := validator.Validate(cfg); err != nil {
		if strings.Contains(err.Error(), "DockerAPIVersion") {
			return nil, ErrInvalidDockerAPIVersion
		}

		return nil, err
	}

	if err := cfg.setSOPSconfig(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// setSOPSconfig verifies and sets the SOPS configuration
func (cfg *AppConfig) setSOPSconfig() error {
	if cfg.SOPSKeyService != "" && cfg.SOPSKeyID == "" && cfg.SOPSKeyFile == "" && cfg.SOPSKey == "" {
		return errors.New("SOPS_KEY_ID, SOPS_KEY_FILE, or SOPS_KEY_BASE64 must be provided when SOPS_KEY_SERVICE is set")
	}

	if cfg.SOPSKeyService == "" && (cfg.SOPSKeyID != "" || cfg.SOPSKeyFile != "" || cfg.SOPSKey != "") {
		return errors.New("SOPS_KEY_SERVICE must be provided when SOPS_KEY_ID, SOPS_KEY_FILE, or SOPS_KEY_BASE64 is set")
	}

	if cfg.SOPSKeyService != "" && (cfg.SOPSKeyID != "" && cfg.SOPSKeyFile != "" && cfg.SOPSKey != "") {
		return errors.New("only one of SOPS_KEY_ID, SOPS_KEY_FILE, or SOPS_KEY_BASE64 can be provided when SOPS_KEY_SERVICE is set")
	}

	// Decode the base64-encoded SOPS key if provided
	if cfg.SOPSKey != "" {
		keyData, err := base64.StdEncoding.DecodeString(cfg.SOPSKey)
		if err != nil {
			return fmt.Errorf("failed to read base64-encoded SOPS key: %v", err)
		}

		cfg.SOPSKey = string(keyData)
	}

	// Read the SOPS key file if provided and set the SOPS key
	if cfg.SOPSKeyFile != "" {
		keyData, err := os.ReadFile(cfg.SOPSKeyFile)
		if err != nil {
			return fmt.Errorf("failed to read SOPS key file: %v", err)
		}

		cfg.SOPSKey = string(keyData)
	}

	return nil
}
