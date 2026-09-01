package docker

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/docker/cli/cli/command"
	contextstore "github.com/docker/cli/cli/context/store"
	"github.com/moby/moby/client"
)

type contextRegistryTestCli struct {
	command.Cli
	apiClient client.APIClient
}

func (c contextRegistryTestCli) Client() client.APIClient {
	return c.apiClient
}

type contextRegistryTestClient struct {
	client.APIClient
	closed    bool
	infoCalls int
}

func (c *contextRegistryTestClient) Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
	c.infoCalls++
	return client.SystemInfoResult{}, nil
}

func (c *contextRegistryTestClient) Close() error {
	c.closed = true
	return nil
}

func TestContextName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input      string
		normalized string
		display    string
	}{
		{"", "", "default"},
		{" default ", "", "default"},
		{"DEFAULT", "", "default"},
		{" remote ", "remote", "remote"},
	}

	for _, tt := range tests {
		if got := NormalizeContextName(tt.input); got != tt.normalized {
			t.Fatalf("NormalizeContextName(%q) = %q, want %q", tt.input, got, tt.normalized)
		}

		if got := DisplayContextName(tt.input); got != tt.display {
			t.Fatalf("DisplayContextName(%q) = %q, want %q", tt.input, got, tt.display)
		}
	}
}

func TestContextRegistryListsAndCachesContexts(t *testing.T) {
	t.Parallel()

	defaultClient := &contextRegistryTestClient{}
	remoteClient := &contextRegistryTestClient{}
	baseCli := contextRegistryTestCli{apiClient: defaultClient}
	remoteCli := contextRegistryTestCli{apiClient: remoteClient}

	registry := NewContextRegistry(baseCli, ContextRegistryOptions{Quiet: true, SwarmFeatures: true})
	registry.listContexts = func() ([]contextstore.Metadata, error) {
		return []contextstore.Metadata{{Name: "remote"}, {Name: "default"}}, nil
	}

	var createCalls int

	registry.createCli = func(_ bool, name string) (command.Cli, error) {
		createCalls++

		if name != "remote" {
			t.Fatalf("unexpected context creation: %s", name)
		}

		return remoteCli, nil
	}
	registry.resolveSwarm = func(_ context.Context, _ client.APIClient) (bool, error) {
		return false, nil
	}

	names, err := registry.Names()
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"", "remote"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("Names() = %#v, want %#v", names, want)
	}

	for range 2 {
		entry, getErr := registry.Get(t.Context(), "remote")
		if getErr != nil {
			t.Fatal(getErr)
		}

		if entry.Cli != remoteCli {
			t.Fatal("expected cached remote cli")
		}
	}

	if createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", createCalls)
	}

	if err = registry.Close(); err != nil {
		t.Fatal(err)
	}

	if !remoteClient.closed {
		t.Fatal("expected remote client to be closed")
	}

	if defaultClient.closed {
		t.Fatal("default client must remain owned by the caller")
	}
}

func TestContextRegistryIsolatesContextErrors(t *testing.T) {
	t.Parallel()

	baseCli := contextRegistryTestCli{apiClient: &contextRegistryTestClient{}}
	registry := NewContextRegistry(baseCli, ContextRegistryOptions{Quiet: true, SwarmFeatures: true})
	registry.listContexts = func() ([]contextstore.Metadata, error) {
		return []contextstore.Metadata{{Name: "broken"}}, nil
	}

	registry.createCli = func(_ bool, _ string) (command.Cli, error) {
		return nil, errors.New("create failed")
	}
	registry.resolveSwarm = func(_ context.Context, _ client.APIClient) (bool, error) {
		return false, nil
	}

	results, err := registry.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	for _, result := range results {
		switch result.Name {
		case "":
			if result.Err != nil {
				t.Fatalf("default context unexpectedly failed: %v", result.Err)
			}
		case "broken":
			if result.Err == nil {
				t.Fatal("expected broken context error")
			}
		default:
			t.Fatalf("unexpected context %q", result.Name)
		}
	}

	if _, err = registry.Get(t.Context(), "missing"); !errors.Is(err, ErrDockerContextNotFound) {
		t.Fatalf("expected ErrDockerContextNotFound, got %v", err)
	}
}

func TestContextRegistryDefaultDoesNotRequireContextListing(t *testing.T) {
	t.Parallel()

	baseCli := contextRegistryTestCli{apiClient: &contextRegistryTestClient{}}
	registry := NewContextRegistry(baseCli, ContextRegistryOptions{Quiet: true, SwarmFeatures: true})
	registry.listContexts = func() ([]contextstore.Metadata, error) {
		return nil, errors.New("context store unavailable")
	}

	registry.resolveSwarm = func(_ context.Context, _ client.APIClient) (bool, error) {
		return false, nil
	}

	entry, err := registry.Get(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}

	if entry.Cli != baseCli || entry.DisplayName() != "default" {
		t.Fatalf("unexpected default entry: %#v", entry)
	}
}

func TestContextRegistryCanDisableSwarmCapabilities(t *testing.T) {
	t.Parallel()

	apiClient := &contextRegistryTestClient{}
	baseCli := contextRegistryTestCli{apiClient: apiClient}
	registry := NewContextRegistry(baseCli, ContextRegistryOptions{SwarmFeatures: false})

	entry, err := registry.Get(t.Context(), DefaultContextName)
	if err != nil {
		t.Fatal(err)
	}

	if entry.SwarmMode {
		t.Fatal("expected Swarm capability to be disabled")
	}

	if apiClient.infoCalls != 0 {
		t.Fatalf("Docker info calls = %d, want 0", apiClient.infoCalls)
	}
}
