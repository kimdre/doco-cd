package graceful

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type testServer struct {
	name     string
	serve    func(ctx context.Context) error
	shutdown func(ctx context.Context) error
}

func (s *testServer) Name() string {
	return s.name
}

func (s *testServer) Serve(ctx context.Context) error {
	return s.serve(ctx)
}

func (s *testServer) Shutdown(ctx context.Context) error {
	return s.shutdown(ctx)
}

func getLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

func TestServe_ShutsDownServersOnStop(t *testing.T) {
	handler := newHandler()

	shutdownCalled := make(chan struct{})

	handler.RegisterServerFunc("svc1",
		func(_ context.Context) error {
			return nil
		},
		func(_ context.Context) error {
			close(shutdownCalled)
			return nil
		},
	)

	shutdownCalled2 := make(chan struct{})

	handler.RegisterServer(&testServer{
		name: "svc2",
		serve: func(_ context.Context) error {
			return nil
		},
		shutdown: func(_ context.Context) error {
			close(shutdownCalled2)
			return nil
		},
	})

	log := getLog()
	if err := handler.Serve(log); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	select {
	case <-shutdownCalled:
		// success
	default:
		t.Fatal("expected shutdown to be called")
	}

	select {
	case <-shutdownCalled2:
		// success
	default:
		t.Fatal("expected shutdown to be called")
	}
}

func TestServeRecoversFromPanic(t *testing.T) {
	handler := newHandler()

	shutdownCalled := make(chan struct{})

	handler.RegisterServerFunc("panic",
		func(_ context.Context) error {
			panic("test panic")
		},
		func(_ context.Context) error {
			close(shutdownCalled)
			return nil
		},
	)

	shutdownCalled2 := make(chan struct{})

	handler.RegisterServer(&testServer{
		name: "svc2",
		serve: func(_ context.Context) error {
			<-shutdownCalled2
			return nil
		},
		shutdown: func(_ context.Context) error {
			close(shutdownCalled2)
			return nil
		},
	})

	log := getLog()
	if err := handler.Serve(log); !strings.Contains(err.Error(), "panic server.Serve panic: test panic") {
		t.Fatalf("expected error, got %v", err)
	}

	select {
	case <-shutdownCalled:
		// success
	default:
		t.Fatal("expected shutdown to be called")
	}

	select {
	case <-shutdownCalled2:
		// success
	default:
		t.Fatal("expected shutdown to be called")
	}
}

func TestServe_ReturnsCombinedErrors(t *testing.T) {
	handler := newHandler()

	handler.RegisterServerFunc("error-server",
		func(_ context.Context) error {
			return errors.New("serve failure")
		},
		func(_ context.Context) error {
			return errors.New("shutdown failure")
		},
	)

	log := getLog()

	err := handler.Serve(log)
	if err == nil {
		t.Fatal("expected error")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "serve failure") {
		t.Fatalf("expected serve failure in error, got %q", errMsg)
	}

	if !strings.Contains(errMsg, "shutdown failure") {
		t.Fatalf("expected shutdown failure in error, got %q", errMsg)
	}
}

func TestServe_HandlesSignalAndShutsDown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping signal test in short mode")
	}

	handler := newHandler()

	shutdownCalled := make(chan struct{})
	stopServe := make(chan struct{})

	handler.RegisterServer(&testServer{
		name: "signal-server",
		serve: func(ctx context.Context) error {
			select {
			case <-stopServe:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		shutdown: func(_ context.Context) error {
			close(shutdownCalled)
			close(stopServe)

			return nil
		},
	})

	log := getLog()
	result := make(chan error, 1)

	wg := sync.WaitGroup{}
	defer wg.Wait()

	wg.Go(func() {
		result <- handler.Serve(log)
	})

	// Wait for Serve to start and register the signal handler.
	time.Sleep(50 * time.Millisecond)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("failed to send SIGTERM: %v", err)
	}

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("expected no error from Serve, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after SIGTERM")
	}

	select {
	case <-shutdownCalled:
		// success
	default:
		t.Fatal("expected shutdown to be called")
	}
}

func TestServe_MultipleServersShutdownConcurrently(t *testing.T) {
	handler := newHandler()

	shutdownEvents := make(chan string, 2)
	stopFirst := make(chan struct{})
	closeFirst := sync.Once{}

	handler.RegisterServer(&testServer{
		name: "blocking-server",
		serve: func(_ context.Context) error {
			<-stopFirst
			return nil
		},
		shutdown: func(_ context.Context) error {
			closeFirst.Do(func() { close(stopFirst) })

			shutdownEvents <- "blocking"

			return errors.New("blocking shutdown failure")
		},
	})

	handler.RegisterServer(&testServer{
		name: "error-server",
		serve: func(_ context.Context) error {
			return errors.New("serve failure")
		},
		shutdown: func(_ context.Context) error {
			shutdownEvents <- "error"
			return errors.New("error shutdown failure")
		},
	})

	log := getLog()

	err := handler.Serve(log)
	if err == nil {
		t.Fatal("expected error")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "serve failure") {
		t.Fatalf("expected serve failure in error, got %q", errMsg)
	}

	if !strings.Contains(errMsg, "blocking shutdown failure") {
		t.Fatalf("expected blocking shutdown failure in error, got %q", errMsg)
	}

	if !strings.Contains(errMsg, "error shutdown failure") {
		t.Fatalf("expected error shutdown failure in error, got %q", errMsg)
	}

	received := map[string]bool{}

	for i := 0; i < 2; i++ {
		select {
		case event := <-shutdownEvents:
			received[event] = true
		case <-time.After(2 * time.Second):
			t.Fatal("expected both servers to be shutdown")
		}
	}

	if !received["blocking"] || !received["error"] {
		t.Fatalf("expected shutdown of both servers, got %v", received)
	}
}
