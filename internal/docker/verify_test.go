package docker

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

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
