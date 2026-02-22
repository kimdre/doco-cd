package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	prococo "github.com/prometheus/common/config"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	SiteUrl           string            `env:"SECRET_PROVIDER_SITE_URL,notEmpty"`         // Endpoint template to query for secrets
	ResultJMESPath    string            `env:"SECRET_PROVIDER_RESULT_JMES_PATH,notEmpty"` // JMESpath query issued against the response data
	RequestBody       string            `env:"SECRET_PROVIDER_REQUEST_BODY"`              // HTTP request payload template
	CustomHeadersJSON string            `env:"SECRET_PROVIDER_CUSTOM_HEADERS"`            // JSON-encoded map of custom HTTP headers to include in requests
	BearerToken       string            `env:"SECRET_PROVIDER_BEARER_TOKEN"`              // #nosec G117 Authentication secret for the Bearer authentication scheme
	BearerTokenFile   string            `env:"SECRET_PROVIDER_BEARER_TOKEN_FILE,file"`    // File path containing the authentication secret for the Bearer authentication scheme
	BasicUsername     string            `env:"SECRET_PROVIDER_BASIC_USERNAME"`            // Authentication principal for the Basic authentication scheme
	BasicUsernameFile string            `env:"SECRET_PROVIDER_BASIC_USERNAME_FILE,file"`  // File path containing the authentication principal for the Basic authentication scheme
	BasicPassword     string            `env:"SECRET_PROVIDER_BASIC_PASSWORD"`            // Authentication secret for the Basic authentication scheme
	BasicPasswordFile string            `env:"SECRET_PROVIDER_BASIC_PASSWORD_FILE,file"`  // File path containing the authentication secret for the Basic authentication scheme
	CustomHeaders     map[string]string `env:"-"`                                         // Parsed custom HTTP headers
}

// GetConfig retrieves and parses the configuration for the Webhook Secrets Provider from environment variables.
func GetConfig() (*Config, error) {
	cfg := Config{}

	mappings := []config.EnvVarFileMapping{
		{EnvName: "SECRET_PROVIDER_BEARER_TOKEN", EnvValue: &cfg.BearerToken, FileValue: &cfg.BearerTokenFile, AllowUnset: true},
		{EnvName: "SECRET_PROVIDER_BASIC_USERNAME", EnvValue: &cfg.BasicUsername, FileValue: &cfg.BasicUsernameFile, AllowUnset: true},
		{EnvName: "SECRET_PROVIDER_BASIC_PASSWORD", EnvValue: &cfg.BasicPassword, FileValue: &cfg.BasicPasswordFile, AllowUnset: true},
	}

	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	if cfg.CustomHeadersJSON != "" {
		if err := json.Unmarshal([]byte(cfg.CustomHeadersJSON), &cfg.CustomHeaders); err != nil {
			return nil, fmt.Errorf("%w: failed to parse custom headers JSON: %w", config.ErrParseConfigFailed, err)
		}
	}

	return &cfg, nil
}

// HTTPBasicAuth returns basic auth credentials created from the internal state.
// If no username is configured, nil is returned. If no password is configured,
// the username is reused as authentication secret.
func (c *Config) HTTPBasicAuth() *prococo.BasicAuth {
	if c.BasicUsername == "" {
		return nil
	}

	result := &prococo.BasicAuth{
		Username: c.BasicUsername,
	}

	if c.BasicPassword == "" {
		result.Password = prococo.Secret(c.BasicUsername)
	} else {
		result.Password = prococo.Secret(c.BasicPassword)
	}

	return result
}

// HTTPAuthorization returns bearer auth credentials created from the internal state.
// If no bearer token is configured, nil is returned.
func (c *Config) HTTPAuthorization() *prococo.Authorization {
	if c.BearerToken == "" {
		return nil
	}

	result := &prococo.Authorization{
		Type:        "Bearer",
		Credentials: prococo.Secret(c.BearerToken),
	}

	return result
}

func (c *Config) NewRoundTripperWithContext(ctx context.Context) (http.RoundTripper, error) {
	httpcfg := prococo.HTTPClientConfig{
		BasicAuth:     c.HTTPBasicAuth(),
		Authorization: c.HTTPAuthorization(),
	}
	if err := httpcfg.Validate(); err != nil {
		return nil, err
	}

	return prococo.NewRoundTripperFromConfigWithContext(ctx, httpcfg,
		"secretprovider-webhook", prococo.WithUserAgent(config.AppName+"/"+config.AppVersion))
}
