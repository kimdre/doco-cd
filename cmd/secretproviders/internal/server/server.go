// Package server implements the shared gRPC harness used by all
// out-of-process secret-provider plugin binaries.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"

	secretproviderv1 "github.com/kimdre/doco-cd/api/secretprovider/v1"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// Provider is the in-process interface plugin binaries implement; the harness
// adapts it to the gRPC SecretProviderServer surface.
type Provider interface {
	Name() string
	GetSecret(ctx context.Context, id string) (string, error)
	GetSecrets(ctx context.Context, ids []string) (map[string]string, error)
	ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error)
	Close()
}

// Options configures Serve.
type Options struct {
	// Endpoint is the address to listen on (unix:///path or tcp://host:port).
	Endpoint string
	// GracePeriod is the maximum duration GracefulStop is allowed to take
	// before the server is forcibly stopped.
	GracePeriod time.Duration
}

const defaultGracePeriod = 5 * time.Second

// Serve listens on the configured endpoint and serves the gRPC plugin until
// ctx is cancelled.
func Serve(ctx context.Context, opts Options, p Provider) error {
	if p == nil {
		return errors.New("provider is required")
	}

	if opts.Endpoint == "" {
		return errors.New("endpoint is required")
	}

	if opts.GracePeriod <= 0 {
		opts.GracePeriod = defaultGracePeriod
	}

	lis, cleanup, err := listener(ctx, opts.Endpoint)
	if err != nil {
		return fmt.Errorf("failed to create listener for %q: %w", opts.Endpoint, err)
	}
	defer cleanup()

	srv := grpc.NewServer()
	secretproviderv1.RegisterSecretProviderServer(srv, newGRPCServer(p))

	var wg sync.WaitGroup

	wg.Add(1)

	go shutdown(ctx, &wg, srv, opts.GracePeriod)

	if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("gRPC server failed: %w", err)
	}

	wg.Wait()
	p.Close()

	return nil
}

func listener(ctx context.Context, endpoint string) (net.Listener, func(), error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}

	var (
		network string
		addr    string
	)

	switch u.Scheme {
	case "unix":
		if u.Path == "" {
			return nil, nil, errors.New("unix endpoint missing path")
		}

		network = "unix"
		addr = u.Path

		_ = os.Remove(addr)
	case "tcp", "":
		network = "tcp"
		addr = u.Host

		if addr == "" {
			addr = u.Path
		}

		if addr == "" {
			return nil, nil, errors.New("tcp endpoint missing host")
		}
	default:
		return nil, nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}

	lc := net.ListenConfig{}

	l, err := lc.Listen(ctx, network, addr)
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		if network == "unix" {
			_ = os.Remove(addr)
		}
	}

	return l, cleanup, nil
}

func shutdown(ctx context.Context, wg *sync.WaitGroup, srv *grpc.Server, grace time.Duration) {
	defer wg.Done()

	<-ctx.Done()

	done := make(chan struct{})

	go func() {
		srv.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(grace):
		srv.Stop()
	}
}

type grpcServer struct {
	secretproviderv1.UnimplementedSecretProviderServer

	p Provider
}

func newGRPCServer(p Provider) *grpcServer {
	return &grpcServer{p: p}
}

func (s *grpcServer) Name(_ context.Context, _ *secretproviderv1.NameRequest) (*secretproviderv1.NameResponse, error) {
	return &secretproviderv1.NameResponse{Name: s.p.Name()}, nil
}

func (s *grpcServer) GetSecret(ctx context.Context, req *secretproviderv1.GetSecretRequest) (*secretproviderv1.GetSecretResponse, error) {
	value, err := s.p.GetSecret(ctx, req.GetId())
	if err != nil {
		return nil, err
	}

	return &secretproviderv1.GetSecretResponse{Value: value}, nil
}

func (s *grpcServer) GetSecrets(ctx context.Context, req *secretproviderv1.GetSecretsRequest) (*secretproviderv1.GetSecretsResponse, error) {
	secrets, err := s.p.GetSecrets(ctx, req.GetIds())
	if err != nil {
		return nil, err
	}

	return &secretproviderv1.GetSecretsResponse{Secrets: secrets}, nil
}

func (s *grpcServer) ResolveSecretReferences(ctx context.Context, req *secretproviderv1.ResolveSecretReferencesRequest) (*secretproviderv1.ResolveSecretReferencesResponse, error) {
	secrets, err := s.p.ResolveSecretReferences(ctx, req.GetSecrets())
	if err != nil {
		return nil, err
	}

	return &secretproviderv1.ResolveSecretReferencesResponse{Secrets: secrets}, nil
}
