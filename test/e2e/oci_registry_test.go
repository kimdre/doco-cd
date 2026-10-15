//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/uuid"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
)

type ociRegistryContainerFake struct {
	testcontainers.Container
	mappedPort func(context.Context, string) (network.Port, error)
}

func (*ociRegistryContainerFake) GetContainerID() string {
	return "unit-registry"
}

func (c *ociRegistryContainerFake) MappedPort(ctx context.Context, port string) (network.Port, error) {
	return c.mappedPort(ctx, port)
}

type ociRegistryDockerFake struct {
	client.APIClient
	disconnect func(context.Context, string, client.NetworkDisconnectOptions) (client.NetworkDisconnectResult, error)
}

func (d *ociRegistryDockerFake) NetworkDisconnect(ctx context.Context, netName string, opts client.NetworkDisconnectOptions) (client.NetworkDisconnectResult, error) {
	return d.disconnect(ctx, netName, opts)
}

func ociRegistryListener(t *testing.T) (string, network.Port) {
	t.Helper()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for OCI registry readiness: %v", err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	t.Cleanup(func() {
		_ = listener.Close()

		<-done
	})

	addr := listener.Addr().String()

	_, portNumber, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}

	port, err := network.ParsePort(portNumber + "/tcp")
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}

	return addr, port
}

func TestOCIRegistryUnitRepushRefreshesCachedNetwork(t *testing.T) {
	t.Parallel()

	firstAddr, firstPort := ociRegistryListener(t)
	secondAddr, secondPort := ociRegistryListener(t)
	ports := []network.Port{firstPort, secondPort}
	resolved := 0
	reg := &ociTestRegistry{
		container: &ociRegistryContainerFake{
			mappedPort: func(_ context.Context, port string) (network.Port, error) {
				if port != ociRegistryPort+"/tcp" {
					return network.Port{}, fmt.Errorf("mapped port = %q, want %s/tcp", port, ociRegistryPort)
				}

				if resolved >= len(ports) {
					return network.Port{}, errors.New("unexpected mapped port lookup")
				}

				current := ports[resolved]
				resolved++

				return current, nil
			},
		},
		hostAddr: "127.0.0.1:1",
		ips:      map[string]string{"cached": "172.20.0.2"},
	}

	for _, addr := range []string{firstAddr, secondAddr} {
		called := false

		ref, err := reg.pushArtifact(t.Context(), "cached", "unit-refresh", "latest", func(ref name.Reference) error {
			called = true

			if got, want := ref.Name(), addr+"/unit-refresh:latest"; got != want {
				return fmt.Errorf("push reference = %q, want %q", got, want)
			}

			return nil
		})
		if err != nil {
			t.Fatalf("push to cached network: %v", err)
		}

		if !called {
			t.Fatal("push callback was not invoked")
		}

		if want := "172.20.0.2:5000/unit-refresh:latest"; ref != want {
			t.Fatalf("network reference = %q, want %q", ref, want)
		}

		if reg.hostAddr != addr {
			t.Fatalf("cached host address = %q, want %q", reg.hostAddr, addr)
		}
	}

	if resolved != 2 {
		t.Fatalf("mapped port lookups = %d, want 2", resolved)
	}
}

func TestOCIRegistryUnitDisconnectSerializesPush(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	addr, port := ociRegistryListener(t)
	disconnectStarted := make(chan struct{})
	allowDisconnect := make(chan struct{})
	disconnected := make(chan struct{})
	reg := &ociTestRegistry{
		container: &ociRegistryContainerFake{
			mappedPort: func(context.Context, string) (network.Port, error) {
				select {
				case <-disconnected:
					return port, nil
				default:
					return network.Port{}, errors.New("port lookup overlapped disconnect")
				}
			},
		},
		hostAddr: addr,
		ips:      map[string]string{"leaving": "172.20.0.2", "surviving": "172.21.0.2"},
	}
	reg.docker = &ociRegistryDockerFake{
		disconnect: func(ctx context.Context, netName string, opts client.NetworkDisconnectOptions) (client.NetworkDisconnectResult, error) {
			close(disconnectStarted)

			select {
			case <-allowDisconnect:
			case <-ctx.Done():
				return client.NetworkDisconnectResult{}, ctx.Err()
			}

			if netName != "leaving" || opts.Container != "unit-registry" || !opts.Force {
				return client.NetworkDisconnectResult{}, fmt.Errorf("unexpected disconnect: %s, %+v", netName, opts)
			}

			close(disconnected)

			return client.NetworkDisconnectResult{}, nil
		},
	}

	detachDone := make(chan error, 1)

	go func() {
		detachDone <- reg.detachFromNetwork(ctx, "leaving")
	}()

	select {
	case <-disconnectStarted:
	case <-ctx.Done():
		t.Fatalf("disconnect did not start: %v", ctx.Err())
	}

	// Docker is blocked inside NetworkDisconnect, not before or after it.
	if reg.mu.TryLock() {
		reg.mu.Unlock()
		t.Fatal("registry mutex was released during NetworkDisconnect")
	}

	pushStarted := make(chan struct{})
	pushDone := make(chan error, 1)

	go func() {
		close(pushStarted)

		_, err := reg.pushArtifact(ctx, "surviving", "unit-serialized", "latest", func(name.Reference) error {
			select {
			case <-disconnected:
				return nil
			default:
				return errors.New("push overlapped disconnect")
			}
		})
		pushDone <- err
	}()

	<-pushStarted
	close(allowDisconnect)

	for _, done := range []<-chan error{detachDone, pushDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("serialized registry operation: %v", err)
			}
		case <-ctx.Done():
			t.Fatalf("serialized registry operation did not finish: %v", ctx.Err())
		}
	}

	if _, attached := reg.ips["leaving"]; attached {
		t.Fatal("successful disconnect retained the leaving network")
	}

	if got := reg.ips["surviving"]; got != "172.21.0.2" {
		t.Fatalf("surviving network IP = %q, want 172.21.0.2", got)
	}
}

func TestOCIRegistryUnitFailedDisconnectPreservesCache(t *testing.T) {
	t.Parallel()

	disconnectErr := errors.New("disconnect failed")
	reg := &ociTestRegistry{
		container: &ociRegistryContainerFake{},
		docker: &ociRegistryDockerFake{
			disconnect: func(context.Context, string, client.NetworkDisconnectOptions) (client.NetworkDisconnectResult, error) {
				return client.NetworkDisconnectResult{}, disconnectErr
			},
		},
		ips: map[string]string{"attached": "172.20.0.2"},
	}

	err := reg.detachFromNetwork(t.Context(), "attached")
	if !errors.Is(err, disconnectErr) {
		t.Fatalf("disconnect error = %v, want %v", err, disconnectErr)
	}

	if !strings.Contains(err.Error(), "network attached") {
		t.Fatalf("disconnect error has no network context: %v", err)
	}

	if got := reg.ips["attached"]; got != "172.20.0.2" {
		t.Fatalf("cached IP after failed disconnect = %q, want 172.20.0.2", got)
	}
}

func TestOCIRegistryUnitDetachUnattachedNetwork(t *testing.T) {
	t.Parallel()

	reg := &ociTestRegistry{ips: map[string]string{"attached": "172.20.0.2"}}

	if err := reg.detachFromNetwork(t.Context(), "missing"); err != nil {
		t.Fatalf("detach unattached network: %v", err)
	}

	if got := reg.ips["attached"]; got != "172.20.0.2" {
		t.Fatalf("unrelated cached IP = %q, want 172.20.0.2", got)
	}
}

func TestOCIRegistryUnitPortResolutionFailurePreventsPush(t *testing.T) {
	t.Parallel()

	resolveErr := errors.New("mapped port unavailable")
	reg := &ociTestRegistry{
		container: &ociRegistryContainerFake{
			mappedPort: func(context.Context, string) (network.Port, error) {
				return network.Port{}, resolveErr
			},
		},
		hostAddr: "127.0.0.1:1",
		ips:      map[string]string{"cached": "172.20.0.2"},
	}
	called := false

	ref, err := reg.pushArtifact(t.Context(), "cached", "unit-error", "latest", func(name.Reference) error {
		called = true

		return nil
	})
	if !errors.Is(err, resolveErr) {
		t.Fatalf("push error = %v, want %v", err, resolveErr)
	}

	if !strings.Contains(err.Error(), "network cached") {
		t.Fatalf("push error has no network context: %v", err)
	}

	if called || ref != "" {
		t.Fatalf("port resolution failure invoked push or returned a reference: called=%t, ref=%q", called, ref)
	}

	if reg.hostAddr != "127.0.0.1:1" {
		t.Fatalf("failed port resolution changed cached host address: %q", reg.hostAddr)
	}
}

func TestOCIRegistryRepublishAfterPeerDetach(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	dockerCli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("create Docker client: %v", err)
	}

	t.Cleanup(func() {
		_ = dockerCli.Close()
	})

	reg, err := ensureOCIRegistry(ctx, dockerCli)
	if err != nil {
		t.Fatalf("ensure shared OCI registry: %v", err)
	}

	newNetwork := func() *testcontainers.DockerNetwork {
		t.Helper()

		nw, err := tcnetwork.New(ctx)
		if err != nil {
			t.Fatalf("create registry regression network: %v", err)
		}

		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()

			if err := reg.detachFromNetwork(cleanupCtx, nw.Name); err != nil {
				t.Errorf("detach shared registry from owned network %s: %v", nw.Name, err)
			}

			if err := nw.Remove(cleanupCtx); err != nil {
				t.Errorf("remove owned network %s: %v", nw.Name, err)
			}
		})

		return nw
	}

	leaving := newNetwork()
	surviving := newNetwork()
	id := uuid.NewString()
	repo := "oci-registry-republish-" + id
	tag := "republish-" + id

	publish := func(nw *testcontainers.DockerNetwork, content string) (string, string) {
		t.Helper()

		var digest string

		ref, err := reg.pushArtifact(ctx, nw.Name, repo, tag, func(ref name.Reference) error {
			var err error

			digest, err = pushOCIArtifact(ref, map[string]string{"compose.yaml": content})
			if err != nil {
				return err
			}

			descriptor, err := remote.Get(ref, remote.WithContext(ctx))
			if err != nil {
				return fmt.Errorf("read published artifact tag: %w", err)
			}

			if got := descriptor.Digest.String(); got != digest {
				return fmt.Errorf("published tag digest = %s, want %s", got, digest)
			}

			return nil
		})
		if err != nil {
			t.Fatalf("publish on network %s: %v", nw.Name, err)
		}

		return ref, digest
	}

	const initialCompose = "services:\n  app:\n    image: alpine:3.22\n"

	_, initialDigest := publish(leaving, initialCompose)

	survivingRef, attachedDigest := publish(surviving, initialCompose)
	if attachedDigest != initialDigest {
		t.Fatalf("unchanged artifact digest = %s, want %s", attachedDigest, initialDigest)
	}

	if err := reg.detachFromNetwork(ctx, leaving.Name); err != nil {
		t.Fatalf("detach peer network: %v", err)
	}

	updatedRef, updatedDigest := publish(surviving, "services:\n  app:\n    image: alpine:3.21\n")
	if updatedRef != survivingRef {
		t.Fatalf("surviving network reference changed: got %q, want %q", updatedRef, survivingRef)
	}

	if updatedDigest == initialDigest {
		t.Fatalf("republished artifact digest did not change: %s", updatedDigest)
	}
}
