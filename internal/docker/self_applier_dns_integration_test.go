package docker

import (
	"context"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
	internaltest "github.com/kimdre/doco-cd/internal/test"
)

// TestSelfUpdateIntegration_ApplierPreflightUsesNetworkServiceDNS verifies
// the clone can resolve project services before a network-drift handover.
func TestSelfUpdateIntegration_ApplierPreflightUsesNetworkServiceDNS(t *testing.T) {
	requireSelfUpdateIntegrationGate(t)

	ctx := t.Context()
	stackName := internaltest.ConvertTestName(t.Name())
	stack := internaltest.ComposeUp(ctx, t,
		internaltest.WithName(stackName),
		internaltest.WithYAML(`
services:
  app:
    image: alpine:3.22
    command: ["sleep", "600"]
    networks: [backend]
  op-connect-api:
    image: alpine:3.22
    command: ["sh", "-c", "while true; do printf 'ready\\n' | nc -l -p 8080; done"]
    networks: [backend]
networks:
  backend:
`))
	originalID := stack.ServiceContainerID(ctx, t, "app")

	original, err := stack.Client.ContainerInspect(ctx, originalID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	netName := stackName + "_backend"

	liveNetwork, err := stack.Client.NetworkInspect(ctx, netName, client.NetworkInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	cloneOpts := BuildSelfApplierCreate(original.Container, "preflight", stackName, true)
	cloneOpts.Config.Cmd = []string{"sleep", "600"}

	clone, err := stack.Client.ContainerCreate(ctx, cloneOpts)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_, _ = stack.Client.ContainerRemove(context.WithoutCancel(ctx), clone.ID, client.ContainerRemoveOptions{Force: true})
	})

	if err := connectSelfApplierNetworks(ctx, stack.Client, clone.ID, original.Container, nil); err != nil {
		t.Fatalf("join project network before starting applier: %v", err)
	}

	if _, err := stack.Client.ContainerStart(ctx, clone.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}

	reachesProvider := func() bool {
		exec, execErr := stack.Client.ExecCreate(ctx, clone.ID, client.ExecCreateOptions{
			Cmd: []string{"nc", "-z", "-w", "2", "op-connect-api", "8080"},
		})
		if execErr != nil {
			t.Fatal(execErr)
		}

		if _, execErr = stack.Client.ExecStart(ctx, exec.ID, client.ExecStartOptions{}); execErr != nil {
			t.Fatal(execErr)
		}

		status, execErr := stack.Client.ExecInspect(ctx, exec.ID, client.ExecInspectOptions{})
		if execErr != nil {
			t.Fatal(execErr)
		}

		return status.ExitCode == 0
	}

	deadline := time.Now().Add(10 * time.Second)
	for !reachesProvider() {
		if time.Now().After(deadline) {
			t.Fatal("applier could not resolve op-connect-api on the Compose network during preflight")
		}

		time.Sleep(250 * time.Millisecond)
	}

	record := selfupdate.Record{
		Predecessor: selfupdate.ContainerRef{ID: originalID},
		Applier:     selfupdate.ContainerRef{ID: clone.ID},
		Drift: &selfupdate.DriftSnapshot{
			Networks:   map[string]network.Inspect{netName: liveNetwork.Network},
			Containers: map[string]container.InspectResponse{originalID: original.Container},
		},
	}
	if err := detachSelfApplierProjectNetworks(ctx, stack.Client, record); err != nil {
		t.Fatal(err)
	}

	if err := ensureSelfApplierPreflightNetworks(ctx, stack.Client, record); err != nil {
		t.Fatalf("restart after detach could not restore service DNS: %v", err)
	}

	if !reachesProvider() {
		t.Fatal("restarted applier could not use network-hosted secret provider")
	}

	if err := detachSelfApplierProjectNetworks(ctx, stack.Client, record); err != nil {
		t.Fatal(err)
	}

	final, err := stack.Client.ContainerInspect(ctx, clone.ID, client.ContainerInspectOptions{})
	if err != nil || final.Container.NetworkSettings == nil ||
		final.Container.NetworkSettings.Networks["bridge"] == nil ||
		final.Container.NetworkSettings.Networks[netName] != nil {
		t.Fatalf("applier not stable for drift: %+v (%v)", final.Container.NetworkSettings, err)
	}
}
