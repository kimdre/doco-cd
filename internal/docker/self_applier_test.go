package docker

import (
	"strings"
	"testing"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

func applierSourceInspect() container.InspectResponse {
	inspect := container.InspectResponse{}
	inspect.Name = "/doco-cd"
	inspect.Config = &container.Config{
		Image:        "ghcr.io/kimdre/doco-cd:1.2.3",
		Cmd:          nil,
		Hostname:     "abc123",
		ExposedPorts: network.PortSet{network.MustParsePort("80/tcp"): {}},
		Healthcheck:  &container.HealthConfig{Test: []string{"CMD", "/doco-cd", "healthcheck"}},
		Labels: map[string]string{
			api.ProjectLabel:                  "doco-cd",
			api.ServiceLabel:                  "app",
			api.ConfigHashLabel:               "hash",
			DocoCDLabels.Deployment.Name:      "doco-cd",
			DocoCDLabels.Metadata.Manager:     "doco-cd",
			DocoCDLabels.Deployment.CommitSHA: "abc",
			"com.example.keep":                "yes",
		},
	}
	inspect.HostConfig = &container.HostConfig{
		Binds:        []string{"/var/run/docker.sock:/var/run/docker.sock"},
		NetworkMode:  "doco-cd_default",
		PortBindings: network.PortMap{network.MustParsePort("80/tcp"): {{HostPort: "80"}}},
		AutoRemove:   true,
	}
	inspect.NetworkSettings = &container.NetworkSettings{
		Networks: map[string]*network.EndpointSettings{
			"doco-cd_default": {Aliases: []string{"app", "doco-cd"}},
		},
	}

	return inspect
}

func TestBuildSelfApplierCreate(t *testing.T) {
	t.Parallel()

	opts := BuildSelfApplierCreate(applierSourceInspect(), "run-1", "doco-cd")

	if got := opts.Config.Cmd; len(got) != 2 || got[0] != "apply-self" || got[1] != "run-1" {
		t.Errorf("cmd = %v, want [apply-self run-1]", got)
	}

	for key := range opts.Config.Labels {
		if strings.HasPrefix(key, "com.docker.compose.") {
			t.Errorf("clone kept compose label %q and could be mistaken for a replica", key)
		}

		if strings.HasPrefix(key, "cd.doco.") && key != SelfApplierLabel && key != SelfStackLabel {
			t.Errorf("clone kept doco-cd label %q and could be reaped by reconciliation", key)
		}
	}

	if got := opts.Config.Labels[SelfApplierLabel]; got != "run-1" {
		t.Errorf("applier label = %q, want %q", got, "run-1")
	}

	if got := opts.Config.Labels[SelfStackLabel]; got != "doco-cd" {
		t.Errorf("stack label = %q, want %q", got, "doco-cd")
	}

	if got := opts.Config.Labels["com.example.keep"]; got != "yes" {
		t.Error("clone dropped an unrelated label")
	}

	if opts.Config.ExposedPorts != nil {
		t.Error("clone kept exposed ports and would collide with the running instance")
	}

	if opts.HostConfig.PortBindings != nil {
		t.Error("clone kept port bindings and would collide with the running instance")
	}

	if got := opts.Config.Healthcheck.Test; len(got) != 1 || got[0] != "NONE" {
		t.Errorf("healthcheck = %v, want [NONE]", got)
	}

	if opts.HostConfig.AutoRemove {
		t.Error("AutoRemove is set, which Docker rejects together with a restart policy")
	}

	if got := opts.HostConfig.RestartPolicy; got.Name != container.RestartPolicyOnFailure || got.MaximumRetryCount != 0 {
		t.Errorf("restart policy = %+v, want on-failure with unlimited retries", got)
	}

	if opts.Config.Hostname != "" {
		t.Error("clone kept the source hostname")
	}

	if !strings.Contains(opts.Name, "-self-applier-") {
		t.Errorf("name = %q, want a self-applier suffix", opts.Name)
	}

	if opts.NetworkingConfig != nil || opts.HostConfig.NetworkMode != "bridge" {
		t.Errorf("clone must use stable bridge network, got mode %q and endpoints %v",
			opts.HostConfig.NetworkMode, opts.NetworkingConfig)
	}

	if got := opts.HostConfig.Binds; len(got) != 1 {
		t.Errorf("binds = %v, want the source binds", got)
	}
}

func TestBuildSelfApplierCreateKeepsNamespaceNetworkModes(t *testing.T) {
	t.Parallel()

	for _, mode := range []container.NetworkMode{"host", "container:sidecar", "none"} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			inspect := applierSourceInspect()
			inspect.HostConfig.NetworkMode = mode

			opts := BuildSelfApplierCreate(inspect, "run-1", "doco-cd")

			if opts.NetworkingConfig != nil {
				t.Errorf("%s networking must not get an endpoint configuration", mode)
			}

			if opts.HostConfig.NetworkMode != mode {
				t.Errorf("clone network mode = %q, want %q", opts.HostConfig.NetworkMode, mode)
			}
		})
	}
}

func TestRestoreCreateFromSnapshot(t *testing.T) {
	t.Parallel()

	snap := applierSourceInspect()
	snap.Image = "sha256:deadbeef"
	snap.NetworkSettings.Networks["doco-cd_default"].EndpointID = "endpoint"

	opts := RestoreCreateFromSnapshot(snap)

	// Pinning to the image ID means a moved tag cannot change what comes back.
	if opts.Config.Image != "sha256:deadbeef" {
		t.Errorf("image = %q, want the snapshot's image ID", opts.Config.Image)
	}

	if opts.Name != "doco-cd" {
		t.Errorf("name = %q, want the original name without a slash", opts.Name)
	}

	endpoint := opts.NetworkingConfig.EndpointsConfig["doco-cd_default"]
	if len(endpoint.Aliases) != 2 {
		t.Errorf("aliases = %v, want the original aliases", endpoint.Aliases)
	}

	if endpoint.EndpointID != "" {
		t.Error("restore carried runtime endpoint state, which Docker assigns itself")
	}
}
