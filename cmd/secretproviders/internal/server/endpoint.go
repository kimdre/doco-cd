package server

import (
	"os"
	"strings"
)

const (
	EnvEndpoint     = "SECRET_PROVIDER_GRPC_ENDPOINT"
	DefaultEndpoint = "unix:///var/run/doco-cd/secret-provider.sock"
)

// EndpointFromEnv returns the endpoint to listen on, falling back to the
// well-known default if the variable is unset.
func EndpointFromEnv() string {
	v, ok := os.LookupEnv(EnvEndpoint)
	if !ok {
		return DefaultEndpoint
	}

	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultEndpoint
	}

	return v
}
