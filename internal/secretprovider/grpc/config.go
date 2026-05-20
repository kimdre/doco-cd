package grpc

import (
	"fmt"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
)

// Config controls how the gRPC secret-provider client connects to a plugin.
type Config struct {
	// Endpoint is the address of the gRPC plugin.
	// Supported forms:
	//   - unix:///path/to/socket
	//   - tcp://host:port
	//   - host:port
	Endpoint string `env:"SECRET_PROVIDER_GRPC_ENDPOINT,notEmpty"`

	// DialTimeout is the maximum duration the client waits when dialing the plugin.
	DialTimeout time.Duration `env:"SECRET_PROVIDER_GRPC_DIAL_TIMEOUT" envDefault:"10s"`
}

func (c *Config) dialTimeout() time.Duration {
	if c.DialTimeout <= 0 {
		return defaultDialTimeout
	}

	return c.DialTimeout
}

// GetConfig parses the gRPC secret-provider configuration from the environment.
func GetConfig() (*Config, error) {
	cfg := Config{}

	if err := config.ParseConfigFromEnv(&cfg, &[]config.EnvVarFileMapping{}); err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	return &cfg, nil
}
