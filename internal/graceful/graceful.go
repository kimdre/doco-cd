package graceful

import (
	"context"
	"errors"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/kimdre/doco-cd/internal/logger"
)

type Server interface {
	// Name returns the name of the server, used for logging and debugging purposes.
	Name() string
	// Shutdown gracefully shuts down the server without interrupting any active connections.
	Shutdown(ctx context.Context) error
	// Serve starts the server and blocks until the server is stopped or an error occurs.
	// if the server is stopped gracefully, it should return nil.
	// If the server is stopped due to an error, it should return the error.
	// if any server is stopped, all servers will be stopped gracefully.
	Serve(ctx context.Context) error
}

var handler struct {
	servers []Server
}

func RegisterServer(server Server) {
	handler.servers = append(handler.servers, server)
}

type graceFuncServer struct {
	name         string
	serveFunc    func(ctx context.Context) error
	shutdownFunc func(ctx context.Context) error
}

func (s *graceFuncServer) Name() string {
	return s.name
}

func (s *graceFuncServer) Shutdown(ctx context.Context) error {
	return s.shutdownFunc(ctx)
}

func (s *graceFuncServer) Serve(ctx context.Context) error {
	return s.serveFunc(ctx)
}

func RegisterServerFunc(name string, serveFunc func(ctx context.Context) error, shutdownFunc func(ctx context.Context) error) {
	RegisterServer(&graceFuncServer{
		name:         name,
		serveFunc:    serveFunc,
		shutdownFunc: shutdownFunc,
	})
}

type onceChan[T any] struct {
	ch   chan T
	once sync.Once
}

func newOnceChan[T any]() *onceChan[T] {
	return &onceChan[T]{
		ch: make(chan T),
	}
}

func (o *onceChan[T]) Close() {
	o.once.Do(func() {
		close(o.ch)
	})
}

func (o *onceChan[T]) Done() <-chan T {
	return o.ch
}

const shutdownTimeout = 10 * time.Second

// Serve starts all registered servers and waits for a shutdown signal or any server to stop.
// When a shutdown signal is received or any server stops, it will attempt to gracefully shut down all servers.
// It returns an error if any server in encounters an error during serving or shutdown.
// It will wait for all servers to shut down gracefully before returning.
func Serve(log *slog.Logger) error {
	signalCtx, signalStop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer signalStop()

	serveCloseChan := newOnceChan[struct{}]()

	var wg sync.WaitGroup

	serveCtx, serveStop := context.WithCancel(context.Background())
	defer serveStop()

	errChan := make(chan error, len(handler.servers)*2)
	for _, server := range handler.servers {
		wg.Go(func() {
			serverName := server.Name()

			defer func() {
				if r := recover(); r != nil {
					// log the panic if needed
					log.Error("goroutine panicked on server.Serve",
						slog.Any("recover", r),
						slog.String("name", serverName),
					)
				}
			}()

			// Stop all servers if any server is stopped
			defer func() {
				serveCloseChan.Close()
				log.Debug("server.Serve stopped", slog.String("name", serverName))
			}()

			log.Debug("server.Serve started", slog.String("name", serverName))

			if err := server.Serve(serveCtx); err != nil {
				// Log the error if needed
				log.Debug("server.Serve failed", slog.String("name", serverName), logger.ErrAttr(err))

				errChan <- errors.New(serverName + " server error: " + err.Error())
			}
		})
	}

	// Wait for either a shutdown signal or any server to stop
	select {
	case <-signalCtx.Done():
		log.Info("shutdown signal received",
			logger.ErrAttr(signalCtx.Err()),
		)
	case <-serveCloseChan.Done():
		log.Info("some server stopped")
	}

	log.Info("shutting down.")

	// shutdown all servers gracefully
	shutdownCtx, shutdownStop := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownStop()

	for _, server := range handler.servers {
		wg.Go(func() {
			serverName := server.Name()

			defer func() {
				if r := recover(); r != nil {
					// log the panic if needed
					log.Error("goroutine panicked on server.Shutdown",
						slog.Any("recover", r),
						slog.String("name", serverName),
					)
				}
			}()

			log.Debug("call server.Shutdown", slog.String("name", serverName))

			if err := server.Shutdown(shutdownCtx); err != nil {
				log.Debug("server.Shutdown failed",
					slog.String("name", serverName),
					logger.ErrAttr(err),
				)

				errChan <- err
			}
		})
	}

	// wait for servers to shutdown gracefully
	log.Info("Waiting for ongoing requests to finish.")
	wg.Wait()

	serveStop()

	var errs []error

	close(errChan)

	for err := range errChan {
		if err != nil {
			errs = append(errs, err)
		}
	}

	log.Info("Server shut down gracefully.")

	return errors.Join(errs...)
}
