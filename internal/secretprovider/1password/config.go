package onepassword

import (
	"fmt"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	AccessToken      string        `env:"SECRET_PROVIDER_ACCESS_TOKEN"`                                              // #nosec G117 -- Access token for authenticating with the secret provider
	AccessTokenFile  string        `env:"SECRET_PROVIDER_ACCESS_TOKEN_FILE,file"`                                    // Path to a file containing the access token
	ConnectHost      string        `env:"SECRET_PROVIDER_CONNECT_HOST"`                                              // Base URL of the 1Password Connect API
	ConnectToken     string        `env:"SECRET_PROVIDER_CONNECT_TOKEN"`                                             // #nosec G117 -- Access token for authenticating with the Connect API
	ConnectTokenFile string        `env:"SECRET_PROVIDER_CONNECT_TOKEN_FILE,file"`                                   // Path to a file containing the Connect API token
	CacheEnabled     bool          `env:"SECRET_PROVIDER_CACHE_ENABLED,notEmpty" envDefault:"false"`                 // Enables in-memory caching for resolved secrets
	CacheTTL         time.Duration `env:"SECRET_PROVIDER_CACHE_TTL,notEmpty" envDefault:"5m"`                        // Cache TTL for resolved secrets
	CacheMaxSize     int           `env:"SECRET_PROVIDER_CACHE_MAX_SIZE,notEmpty" envDefault:"100" validate:"min=1"` // Maximum number of secrets stored in the in-memory cache
}

func (c Config) UseConnect() bool {
	return c.ConnectHost != "" && c.ConnectToken != ""
}

// GetConfig retrieves and parses the configuration for the Bitwarden Secrets Manager from environment variables.
func GetConfig() (*Config, error) {
	cfg := Config{}

	mappings := []config.EnvVarFileMapping{
		{EnvName: "SECRET_PROVIDER_ACCESS_TOKEN", EnvValue: &cfg.AccessToken, FileValue: &cfg.AccessTokenFile, AllowUnset: true},
		{EnvName: "SECRET_PROVIDER_CONNECT_TOKEN", EnvValue: &cfg.ConnectToken, FileValue: &cfg.ConnectTokenFile, AllowUnset: true},
	}

	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	connectHostSet := cfg.ConnectHost != ""
	connectTokenSet := cfg.ConnectToken != ""

	if connectHostSet != connectTokenSet {
		return nil, fmt.Errorf("%w: SECRET_PROVIDER_CONNECT_HOST and SECRET_PROVIDER_CONNECT_TOKEN (or SECRET_PROVIDER_CONNECT_TOKEN_FILE) must be set together", config.ErrParseConfigFailed)
	}

	if cfg.UseConnect() {
		// Connect Server already caches vault data, so disable local cache in this mode.
		cfg.CacheEnabled = false

		return &cfg, nil
	}

	if cfg.AccessToken == "" {
		return nil, fmt.Errorf("%w: SECRET_PROVIDER_ACCESS_TOKEN (or SECRET_PROVIDER_ACCESS_TOKEN_FILE) is required when SECRET_PROVIDER_CONNECT_HOST/SECRET_PROVIDER_CONNECT_TOKEN are not set", config.ErrParseConfigFailed)
	}

	if cfg.CacheEnabled && cfg.CacheTTL <= 0 {
		return nil, fmt.Errorf("%w: SECRET_PROVIDER_CACHE_TTL must be greater than 0 when cache is enabled", config.ErrParseConfigFailed)
	}

	return &cfg, nil
}
