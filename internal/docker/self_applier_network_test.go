package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

type applierNetworkClient struct {
	client.APIClient
	attached       map[string]*network.EndpointSettings
	networkNames   map[string]string
	connections    []string
	disconnections []string
	disconnectErr  error
}

// ContainerInspect returns the mock applier's current network attachments.
func (c *applierNetworkClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: container.InspectResponse{
		NetworkSettings: &container.NetworkSettings{Networks: c.attached},
	}}, nil
}

// NetworkConnect records preflight attachments in the network mock.
func (c *applierNetworkClient) NetworkConnect(_ context.Context, id string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	if options.Container != "clone" || options.EndpointConfig != nil {
		return client.NetworkConnectResult{}, errors.New("unexpected applier network identity")
	}

	c.connections = append(c.connections, id)
	c.attached[c.networkNames[id]] = &network.EndpointSettings{NetworkID: id}

	return client.NetworkConnectResult{}, nil
}

// NetworkDisconnect records project-network detachment before recreation.
func (c *applierNetworkClient) NetworkDisconnect(_ context.Context, id string, options client.NetworkDisconnectOptions) (client.NetworkDisconnectResult, error) {
	if options.Container != "clone" {
		return client.NetworkDisconnectResult{}, errors.New("unexpected applier container")
	}

	c.disconnections = append(c.disconnections, id)
	if c.disconnectErr != nil {
		return client.NetworkDisconnectResult{}, c.disconnectErr
	}

	delete(c.attached, c.networkNames[id])

	return client.NetworkDisconnectResult{}, nil
}

// TestApplierReattachesForPreflightBeforeDetachingForNetworkDrift checks DNS
// access on retry and safe detachment before network recreation.
func TestApplierReattachesForPreflightBeforeDetachingForNetworkDrift(t *testing.T) {
	previous := applierSourceInspect()
	previous.NetworkSettings.Networks = map[string]*network.EndpointSettings{
		"stack_backend":   {NetworkID: "old-backend", Aliases: []string{"app", "op-connect-api"}},
		"secret_external": {NetworkID: "external", Aliases: []string{"secret-provider"}},
	}
	record := selfupdate.Record{
		Predecessor: selfupdate.ContainerRef{ID: "old"},
		Applier:     selfupdate.ContainerRef{ID: "clone"},
		Drift: &selfupdate.DriftSnapshot{
			Containers: map[string]container.InspectResponse{"old": previous},
			Networks:   map[string]network.Inspect{"stack_backend": {Network: network.Network{ID: "old-backend"}}},
		},
	}

	fake := &applierNetworkClient{
		attached:     map[string]*network.EndpointSettings{"bridge": {NetworkID: "bridge-id"}},
		networkNames: map[string]string{"old-backend": "stack_backend", "external": "secret_external"},
	}
	if err := ensureSelfApplierPreflightNetworks(t.Context(), fake, record); err != nil {
		t.Fatal(err)
	}

	if err := ensureSelfApplierPreflightNetworks(t.Context(), fake, record); err != nil {
		t.Fatal(err)
	}

	if len(fake.connections) != 2 || fake.attached["stack_backend"] == nil ||
		fake.attached["secret_external"] == nil {
		t.Fatalf("preflight did not restore old service DNS: connected %v, networks %v", fake.connections, fake.attached)
	}

	if err := detachSelfApplierProjectNetworks(t.Context(), fake, record); err != nil {
		t.Fatal(err)
	}

	if len(fake.disconnections) != 1 || fake.disconnections[0] != "old-backend" ||
		fake.attached["stack_backend"] != nil || fake.attached["bridge"] == nil ||
		fake.attached["secret_external"] == nil {
		t.Fatalf("drift detach = %v, networks %v; want only project network removed", fake.disconnections, fake.attached)
	}
	// A crash between detach and recording DriftStarted must not strand a
	// restarted applier without the network-hosted secret provider.
	if err := ensureSelfApplierPreflightNetworks(t.Context(), fake, record); err != nil {
		t.Fatal(err)
	}

	if fake.attached["stack_backend"] == nil || len(fake.connections) != 3 {
		t.Fatalf("applier could not recover preflight DNS: %+v, %v", fake.attached, fake.connections)
	}
}

// TestApplierRefusesNetworkDriftWhenStillAttached prevents recreating a
// network that still carries the clone.
func TestApplierRefusesNetworkDriftWhenStillAttached(t *testing.T) {
	record := selfupdate.Record{
		Applier: selfupdate.ContainerRef{ID: "clone"},
		Drift: &selfupdate.DriftSnapshot{Networks: map[string]network.Inspect{
			"stack_backend": {Network: network.Network{ID: "old-backend"}},
		}},
	}

	fake := &applierNetworkClient{
		attached: map[string]*network.EndpointSettings{
			"bridge": {NetworkID: "bridge-id"}, "stack_backend": {NetworkID: "old-backend"},
		},
		networkNames:  map[string]string{"old-backend": "stack_backend"},
		disconnectErr: errors.New("network is busy"),
	}
	if err := detachSelfApplierProjectNetworks(t.Context(), fake, record); err == nil || !strings.Contains(err.Error(), "network is busy") {
		t.Fatalf("detach failure = %v; want refusal to recreate while attached", err)
	}

	if fake.attached["stack_backend"] == nil {
		t.Error("test did not retain a project-network attachment")
	}

	delete(fake.attached, "bridge")

	if err := detachSelfApplierProjectNetworks(t.Context(), fake, record); err == nil || !strings.Contains(err.Error(), "stable bridge") {
		t.Fatalf("missing bridge = %v; want refusal before detaching", err)
	}
}
