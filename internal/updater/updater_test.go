package updater

import (
	"net/netip"
	"testing"

	containerTypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

func TestNormalizeContainerName(t *testing.T) {
	t.Parallel()

	if got := normalizeContainerName("/doco-cd"); got != "doco-cd" {
		t.Fatalf("expected normalized container name to be %q, got %q", "doco-cd", got)
	}
}

func TestBuildCreateOptionsPreservesCoreSettings(t *testing.T) {
	t.Parallel()

	inspect := containerTypes.InspectResponse{
		Name:  "/doco-cd",
		Image: "sha256:old",
		Config: &containerTypes.Config{
			Image:       "ghcr.io/kimdre/doco-cd:latest",
			Env:         []string{"HTTP_PORT=80", "LOG_LEVEL=info"},
			Entrypoint:  []string{"/doco-cd"},
			Cmd:         []string{"healthcheck"},
			Labels:      map[string]string{"test": "value"},
			WorkingDir:  "/",
			StopTimeout: new(15),
			Healthcheck: &containerTypes.HealthConfig{Test: []string{"CMD", "/doco-cd", "healthcheck"}},
		},
		HostConfig: &containerTypes.HostConfig{
			Binds:         []string{"/var/run/docker.sock:/var/run/docker.sock", "data:/data"},
			RestartPolicy: containerTypes.RestartPolicy{Name: "unless-stopped"},
		},
		NetworkSettings: &containerTypes.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"bridge": {Aliases: []string{"doco-cd"}},
			},
		},
	}

	createOptions, err := buildCreateOptions(inspect, "ghcr.io/kimdre/doco-cd:latest", "doco-cd")
	if err != nil {
		t.Fatalf("buildCreateOptions() error = %v", err)
	}

	if createOptions.Name != "doco-cd" {
		t.Fatalf("expected container name %q, got %q", "doco-cd", createOptions.Name)
	}

	if createOptions.Config == nil || createOptions.Config.Image != "ghcr.io/kimdre/doco-cd:latest" {
		t.Fatalf("expected image reference to be preserved")
	}

	if len(createOptions.Config.Env) != 2 {
		t.Fatalf("expected env vars to be preserved, got %d", len(createOptions.Config.Env))
	}

	if createOptions.HostConfig == nil || len(createOptions.HostConfig.Binds) != 2 {
		t.Fatalf("expected bind mounts to be preserved")
	}

	if createOptions.NetworkingConfig == nil || len(createOptions.NetworkingConfig.EndpointsConfig) != 1 {
		t.Fatalf("expected network settings to be preserved")
	}
}

func TestBuildNetworkingConfigStripsRuntimeFields(t *testing.T) {
	t.Parallel()

	settings := &containerTypes.NetworkSettings{
		Networks: map[string]*network.EndpointSettings{
			"app-net": {
				NetworkID:           "network-id",
				EndpointID:          "endpoint-id",
				Gateway:             netip.MustParseAddr("172.18.0.1"),
				IPAddress:           netip.MustParseAddr("172.18.0.2"),
				IPv6Gateway:         netip.MustParseAddr("fd00::1"),
				GlobalIPv6Address:   netip.MustParseAddr("fd00::2"),
				IPPrefixLen:         24,
				GlobalIPv6PrefixLen: 64,
				DNSNames:            []string{"doco-cd"},
				Aliases:             []string{"doco-cd"},
				IPAMConfig: &network.EndpointIPAMConfig{
					IPv4Address: netip.MustParseAddr("172.18.0.10"),
				},
			},
		},
	}

	cfg, err := buildNetworkingConfig(settings)
	if err != nil {
		t.Fatalf("buildNetworkingConfig() error = %v", err)
	}

	endpoint := cfg.EndpointsConfig["app-net"]
	if endpoint.NetworkID != "" || endpoint.EndpointID != "" {
		t.Fatalf("expected runtime network identifiers to be stripped")
	}

	if endpoint.Gateway.IsValid() || endpoint.IPAddress.IsValid() || endpoint.GlobalIPv6Address.IsValid() {
		t.Fatalf("expected runtime IP addresses to be stripped")
	}

	if endpoint.IPPrefixLen != 0 || endpoint.GlobalIPv6PrefixLen != 0 {
		t.Fatalf("expected runtime prefix lengths to be stripped")
	}

	if endpoint.DNSNames != nil {
		t.Fatalf("expected runtime DNS names to be stripped")
	}

	if len(endpoint.Aliases) != 1 || endpoint.IPAMConfig == nil {
		t.Fatalf("expected user-defined endpoint settings to be preserved")
	}
}

func TestIsSwarmManaged(t *testing.T) {
	t.Parallel()

	if !isSwarmManaged(&containerTypes.Config{Labels: map[string]string{"com.docker.swarm.service.name": "doco-cd"}}) {
		t.Fatal("expected swarm-managed config to be detected")
	}

	if isSwarmManaged(&containerTypes.Config{Labels: map[string]string{"test": "value"}}) {
		t.Fatal("expected non-swarm config to be ignored")
	}
}
