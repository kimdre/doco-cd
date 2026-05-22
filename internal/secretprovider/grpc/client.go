// Package grpc implements a SecretProvider that delegates to an out-of-process
// gRPC plugin.
package grpc

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	secretproviderv1 "github.com/kimdre/doco-cd/api/secretprovider/v1"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const (
	Name = "grpc"

	defaultDialTimeout = 10 * time.Second
)

var ErrInvalidEndpoint = errors.New("invalid gRPC secret provider endpoint")

// ValueProvider is a SecretProvider that forwards requests to a gRPC plugin.
type ValueProvider struct {
	conn   *grpc.ClientConn
	client secretproviderv1.SecretProviderClient
}

// NewValueProvider dials the plugin endpoint described by cfg and returns a
// ValueProvider that proxies secret lookups over gRPC.
func NewValueProvider(ctx context.Context, cfg *Config) (*ValueProvider, error) {
	target, err := dialTarget(cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC client for secret provider plugin %q: %w", cfg.Endpoint, err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, cfg.dialTimeout())
	defer cancel()

	client := secretproviderv1.NewSecretProviderClient(conn)
	if _, err := client.Info(probeCtx, &secretproviderv1.InfoRequest{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to reach secret provider plugin %q: %w", cfg.Endpoint, err)
	}

	return &ValueProvider{
		conn:   conn,
		client: client,
	}, nil
}

// Close releases the gRPC connection to the plugin.
func (p *ValueProvider) Close() error {
	if p.conn == nil {
		return nil
	}

	return p.conn.Close()
}

// GetSecret retrieves a secret value from the plugin.
func (p *ValueProvider) GetSecret(ctx context.Context, id string) (string, error) {
	secrets, err := p.GetSecrets(ctx, []string{id})
	if err != nil {
		return "", err
	}

	value, ok := secrets[id]
	if !ok {
		return "", fmt.Errorf("secret %q not returned by plugin", id)
	}

	return value, nil
}

// GetSecrets retrieves multiple secret values from the plugin.
func (p *ValueProvider) GetSecrets(ctx context.Context, ids []string) (map[string]string, error) {
	resp, err := p.client.GetSecrets(ctx, &secretproviderv1.GetSecretsRequest{Ids: ids})
	if err != nil {
		return nil, err
	}

	return resp.GetSecrets(), nil
}

// ResolveSecretReferences resolves a map of environment-variable names to
// secret references via the plugin.
func (p *ValueProvider) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
	resp, err := p.client.ResolveSecretReferences(ctx, &secretproviderv1.ResolveSecretReferencesRequest{Secrets: secrets})
	if err != nil {
		return nil, err
	}

	return resp.GetSecrets(), nil
}

// dialTarget converts an endpoint URI into a target accepted by grpc.Dial.
// Supported schemes: tcp://host:port, unix:///path/to/sock, host:port (treated as tcp).
func dialTarget(endpoint string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("%w: endpoint is empty", ErrInvalidEndpoint)
	}

	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" {
		return endpoint, nil // nolint:nilerr // Treat as host:port if parsing fails or scheme is missing
	}

	switch u.Scheme {
	case "unix":
		if u.Path == "" {
			return "", fmt.Errorf("%w: unix endpoint missing path", ErrInvalidEndpoint)
		}

		return "unix://" + u.Path, nil
	case "tcp":
		if u.Host == "" {
			return "", fmt.Errorf("%w: tcp endpoint missing host", ErrInvalidEndpoint)
		}

		return u.Host, nil
	case "dns", "passthrough":
		return endpoint, nil
	default:
		return "", fmt.Errorf("%w: unsupported scheme %q", ErrInvalidEndpoint, u.Scheme)
	}
}
