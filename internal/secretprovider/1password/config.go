package onepassword

import (
	"fmt"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	AccessToken     string        `env:"SECRET_PROVIDER_ACCESS_TOKEN" validate:"nonzero"`           // #nosec G117 -- Access token for authenticating with the secret provider
	AccessTokenFile string        `env:"SECRET_PROVIDER_ACCESS_TOKEN_FILE,file"`                    // Path to a file containing the access token
	CacheEnabled    bool          `env:"SECRET_PROVIDER_CACHE_ENABLED,notEmpty" envDefault:"false"` // Enables in-memory caching for resolved secrets
	CacheTTL        time.Duration `env:"SECRET_PROVIDER_CACHE_TTL,notEmpty" envDefault:"5m"`        // Cache TTL for resolved secrets
}

// GetConfig retrieves and parses the configuration for the Bitwarden Secrets Manager from environment variables.
func GetConfig() (*Config, error) {
	cfg := Config{}

	mappings := []config.EnvVarFileMapping{
		{EnvName: "SECRET_PROVIDER_ACCESS_TOKEN", EnvValue: &cfg.AccessToken, FileValue: &cfg.AccessTokenFile, AllowUnset: false},
	}

	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	if cfg.CacheEnabled && cfg.CacheTTL <= 0 {
		return nil, fmt.Errorf("%w: SECRET_PROVIDER_CACHE_TTL must be greater than 0 when cache is enabled", config.ErrParseConfigFailed)
	}

	return &cfg, nil
}
