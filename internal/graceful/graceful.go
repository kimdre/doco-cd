package graceful

import (
	"context"
	"errors"
	"fmt"
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

type handler struct {
	lock    sync.Mutex
	servers []Server
	// shutdownFuncs is a list of functions to be called on shutdown
	shutdownFuncs []func()
}

var defaultHandler handler

func init() {
	defaultHandler = newHandler()
}

func newHandler() handler {
	return handler{}
}

// RegisterServer registers a server to be started and stopped gracefully.
func (h *handler) RegisterServer(server Server) {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.servers = append(h.servers, server)
}

// see [handler.RegisterServer].
func RegisterServer(server Server) {
	defaultHandler.RegisterServer(server)
}

// GetServers returns all registered servers.
func (h *handler) GetServers() []Server {
	h.lock.Lock()
	defer h.lock.Unlock()

	return h.servers
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

// RegisterServerFunc registers server and shutdown function to be started and stopped gracefully.
func (h *handler) RegisterServerFunc(name string, serveFunc func(ctx context.Context) error, shutdownFunc func(ctx context.Context) error) {
	h.RegisterServer(&graceFuncServer{
		name:         name,
		serveFunc:    serveFunc,
		shutdownFunc: shutdownFunc,
	})
}

// see [handler.RegisterServerFunc].
func RegisterServerFunc(name string, serveFunc func(ctx context.Context) error, shutdownFunc func(ctx context.Context) error) {
	defaultHandler.RegisterServerFunc(name, serveFunc, shutdownFunc)
}

const shutdownTimeout = 10 * time.Second

// see [handler.Serve].
func Serve(log *slog.Logger) error {
	return defaultHandler.Serve(log)
}

// Serve starts all registered servers and waits for a shutdown signal or any server to stop.
// When a shutdown signal is received or any server stops, it will attempt to gracefully shut down all servers.
// It returns an error if any server in encounters an error during serving or shutdown.
// It will wait for all servers to shut down gracefully before returning.
// It will call all registered shutdown functions before returning.
func (h *handler) Serve(log *slog.Logger) error {
	signalCtx, signalStop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer signalStop()

	serveCloseChan := newOnceChan[struct{}]()

	var wg sync.WaitGroup

	serveCtx, serveStop := context.WithCancel(context.Background())
	defer serveStop()

	servers := h.GetServers()

	errChan := make(chan error, len(servers)*2)
	for _, server := range servers {
		wg.Go(func() {
			serverName := server.Name()

			defer func() {
				if r := recover(); r != nil {
					log.Error("goroutine panicked on server.Serve",
						slog.Any("recover", r),
						slog.String("name", serverName),
					)

					errChan <- fmt.Errorf("%s server.Serve panic: %v", serverName, r)
				}
			}()

			// Stop all servers if any server is stopped
			defer func() {
				serveCloseChan.Close()
				log.Info("server.Serve stopped", slog.String("name", serverName))
			}()

			log.Info("server.Serve started", slog.String("name", serverName))

			if err := server.Serve(serveCtx); err != nil {
				log.Warn("server.Serve failed", slog.String("name", serverName), logger.ErrAttr(err))

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

	for _, server := range servers {
		wg.Go(func() {
			serverName := server.Name()

			defer func() {
				if r := recover(); r != nil {
					log.Error("goroutine panicked on server.Shutdown",
						slog.Any("recover", r),
						slog.String("name", serverName),
					)

					errChan <- fmt.Errorf("%s server.Shutdown panic: %v", serverName, r)
				}
			}()

			log.Info("call server.Shutdown", slog.String("name", serverName))

			if err := server.Shutdown(shutdownCtx); err != nil {
				log.Warn("server.Shutdown failed",
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

	h.runRegisteredShutdownFuncs(log)
	log.Info("server shutdown gracefully.")

	return errors.Join(errs...)
}
