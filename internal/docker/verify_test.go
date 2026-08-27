package docker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyDockerHostConnectionContextErrors(t *testing.T) {
	t.Parallel()

	t.Run("non-200 response", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "daemon exploded", http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)

		host := "tcp://" + strings.TrimPrefix(server.URL, "http://")

		err := VerifyDockerHostConnectionContext(t.Context(), host)
		if err == nil || !strings.Contains(err.Error(), "failed to get docker info") {
			t.Fatalf("error = %v, want docker info failure", err)
		}
	})

	t.Run("unsupported scheme", func(t *testing.T) {
		t.Parallel()

		err := VerifyDockerHostConnectionContext(t.Context(), "http://127.0.0.1:2375")
		if err == nil || !strings.Contains(err.Error(), "unsupported DOCKER_HOST scheme") {
			t.Fatalf("error = %v, want unsupported scheme failure", err)
		}
	})
}

// TestVerifyDockerAPIAccessContextSelectsSentinel verifies that the sentinel
// error identifying the connection kind matches the configured DOCKER_HOST.
func TestVerifyDockerAPIAccessContextSelectsSentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))

	err, errType := VerifyDockerAPIAccessContext(t.Context())
	if err == nil || !errors.Is(errType, ErrDockerHostConnectionFailed) {
		t.Fatalf("err = %v, errType = %v; want docker host sentinel", err, errType)
	}

	t.Setenv("DOCKER_HOST", "")

	_, errType = VerifyDockerAPIAccessContext(t.Context())
	if !errors.Is(errType, ErrDockerSocketConnectionFailed) {
		t.Fatalf("errType = %v; want docker socket sentinel", errType)
	}
}

func TestVerifyDockerHostConnectionContextCancellation(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		network string
		address func(*testing.T) string
		host    func(string) string
	}{
		{
			name:    "TCP",
			network: "tcp",
			address: func(*testing.T) string { return "127.0.0.1:0" },
			host:    func(address string) string { return "tcp://" + address },
		},
		{
			name:    "Unix",
			network: "unix",
			address: func(t *testing.T) string { return filepath.Join(t.TempDir(), "docker.sock") },
			host:    func(address string) string { return "unix://" + address },
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			listener, err := net.Listen(testCase.network, testCase.address(t))
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = listener.Close() })

			accepted := make(chan struct{})

			go func() {
				connection, acceptErr := listener.Accept()
				if acceptErr != nil {
					return
				}

				defer connection.Close() //nolint:errcheck

				close(accepted)

				_, _ = connection.Read(make([]byte, 4096))

				<-t.Context().Done()
			}()

			ctx, cancel := context.WithCancel(t.Context())

			result := make(chan error, 1)
			go func() {
				result <- VerifyDockerHostConnectionContext(ctx, testCase.host(listener.Addr().String()))
			}()

			select {
			case <-accepted:
				cancel()
			case <-time.After(time.Second):
				t.Fatal("Docker health request was not accepted")
			}

			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected context cancellation, got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Docker health request did not stop after cancellation")
			}
		})
	}
}
