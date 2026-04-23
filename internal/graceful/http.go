package graceful

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

type graceHttpServer struct {
	name   string
	server *http.Server
}

func (s *graceHttpServer) Name() string {
	return s.name
}

func (s *graceHttpServer) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *graceHttpServer) Serve(ctx context.Context) error {
	s.server.BaseContext = func(_ net.Listener) context.Context {
		return ctx
	}

	err := s.server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("failed to listen on %v, error: %w", s.server.Addr, err)
	}
	// Server was closed gracefully, no need to return an error
	return nil
}

func NewHttpServer(name string, server *http.Server) Server {
	return &graceHttpServer{
		name:   name,
		server: server,
	}
}
