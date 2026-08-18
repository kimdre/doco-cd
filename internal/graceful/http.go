package graceful

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

type graceHttpServer struct {
	name        string
	server      *http.Server
	tlsCertFile string
	tlsKeyFile  string
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

	var err error

	switch {
	case s.tlsCertFile != "" && s.tlsKeyFile != "":
		err = s.server.ListenAndServeTLS(s.tlsCertFile, s.tlsKeyFile)
	case s.tlsCertFile != "" || s.tlsKeyFile != "":
		// Defensive guard: callers are expected to pass both files together or
		// neither. Fail clearly instead of letting ListenAndServeTLS reject an
		// empty cert/key path with a confusing low-level error.
		return fmt.Errorf("server %q: tlsCertFile and tlsKeyFile must both be set to enable TLS", s.name)
	default:
		err = s.server.ListenAndServe()
	}

	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("failed to listen on %v, error: %w", s.server.Addr, err)
	}
	// Server was closed gracefully, no need to return an error
	return nil
}

func NewHttpServer(name string, server *http.Server, tlsCertFile string, tlsKeyFile string) Server {
	return &graceHttpServer{
		name:        name,
		server:      server,
		tlsCertFile: tlsCertFile,
		tlsKeyFile:  tlsKeyFile,
	}
}
