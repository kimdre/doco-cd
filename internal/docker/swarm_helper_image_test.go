package docker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	distreference "github.com/distribution/reference"
	"github.com/docker/cli/cli/config/configfile"
	configtypes "github.com/docker/cli/cli/config/types"
	"github.com/moby/moby/api/pkg/authconfig"
	"github.com/moby/moby/api/types/registry"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

func TestDefaultSwarmHelperImageIsPinned(t *testing.T) {
	t.Parallel()

	ref, err := distreference.ParseNormalizedNamed(DefaultSwarmHelperImage)
	if err != nil {
		t.Fatalf("parse DefaultSwarmHelperImage: %v", err)
	}

	if distreference.FamiliarName(ref) != "docker" {
		t.Errorf("image name = %q, want docker", distreference.FamiliarName(ref))
	}

	tagged, ok := ref.(distreference.Tagged)
	if !ok || !strings.HasSuffix(tagged.Tag(), "-cli") {
		t.Errorf("DefaultSwarmHelperImage = %q, want a *-cli tag", DefaultSwarmHelperImage)
	}

	if _, ok := ref.(distreference.Canonical); !ok {
		t.Errorf("DefaultSwarmHelperImage = %q, want a digest pin", DefaultSwarmHelperImage)
	}
}

func TestResolveSwarmHelperImage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		image string
		want  string
	}{
		{name: "empty selects default", image: "", want: DefaultSwarmHelperImage},
		{name: "whitespace selects default", image: "  \t", want: DefaultSwarmHelperImage},
		{name: "override is trimmed", image: " registry.example.com/docker:29-cli ", want: "registry.example.com/docker:29-cli"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := resolveSwarmHelperImage(tc.image); got != tc.want {
				t.Errorf("resolveSwarmHelperImage(%q) = %q, want %q", tc.image, got, tc.want)
			}
		})
	}
}

func TestNewSwarmHelperImage(t *testing.T) {
	t.Parallel()

	if got := NewSwarmHelperImage(nil); got != DefaultSwarmHelperImage {
		t.Errorf("NewSwarmHelperImage(nil) = %q, want default", got)
	}

	if got := NewSwarmHelperImage(&app.Config{}); got != DefaultSwarmHelperImage {
		t.Errorf("NewSwarmHelperImage(empty) = %q, want default", got)
	}

	override := "registry.example.com/docker:29-cli"
	if got := NewSwarmHelperImage(&app.Config{DockerSwarmHelperImage: override}); got != override {
		t.Errorf("NewSwarmHelperImage(override) = %q, want %q", got, override)
	}
}

var errSwarmJobSpecStop = errors.New("stop after capture")

// swarmJobSpecClient records the options that RunSwarmJob sends.
// It fails create or update after the capture, so the test does not wait on a job.
type swarmJobSpecClient struct {
	client.APIClient
	existing   *swarmTypes.Service
	createOpts *client.ServiceCreateOptions
	updateOpts *client.ServiceUpdateOptions
}

func (c *swarmJobSpecClient) ServiceCreate(_ context.Context, opts client.ServiceCreateOptions) (client.ServiceCreateResult, error) {
	c.createOpts = &opts

	if c.existing != nil {
		return client.ServiceCreateResult{}, errors.New("service " + opts.Spec.Name + " already exists")
	}

	return client.ServiceCreateResult{}, errSwarmJobSpecStop
}

func (c *swarmJobSpecClient) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{Items: []swarmTypes.Service{*c.existing}}, nil
}

func (c *swarmJobSpecClient) ServiceInspect(context.Context, string, client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	return client.ServiceInspectResult{Service: *c.existing}, nil
}

func (c *swarmJobSpecClient) ServiceUpdate(_ context.Context, _ string, opts client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
	c.updateOpts = &opts

	return client.ServiceUpdateResult{}, errSwarmJobSpecStop
}

func TestRunSwarmJobHelperImageSpec(t *testing.T) {
	t.Parallel()

	const (
		title         = "helper-image-spec"
		registryHost  = "registry.example.com"
		registryUser  = "helper-user"
		overrideImage = registryHost + "/tools/docker:29-cli@sha256:0000000000000000000000000000000000000000000000000000000000000000"
		legacyImage   = "docker:cli@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	)

	newCLI := func(apiClient client.APIClient, missingHelper bool) swarmJobIdentityCLI {
		cfg := configfile.New("")
		if missingHelper {
			cfg.CredentialHelpers = map[string]string{registryHost: "doco-cd-swarm-test-missing"}
		} else {
			cfg.AuthConfigs[registryHost] = configtypes.AuthConfig{
				Username:      registryUser,
				Password:      "helper-secret",
				ServerAddress: registryHost,
			}
		}

		return swarmJobIdentityCLI{apiClient: apiClient, cfg: cfg}
	}

	platforms := []swarmTypes.Platform{{Architecture: "arm64", OS: "linux"}}
	constraints := []string{"node.platform.os == linux"}

	existingService := func(image string) *swarmTypes.Service {
		return &swarmTypes.Service{
			ID: "helper-job-id",
			Spec: swarmTypes.ServiceSpec{
				Name: app.Name + "_" + title,
				TaskTemplate: swarmTypes.TaskSpec{
					ContainerSpec: &swarmTypes.ContainerSpec{Image: image},
					Placement:     &swarmTypes.Placement{Platforms: platforms, Constraints: constraints},
				},
			},
		}
	}

	requireAuthUser := func(t *testing.T, encoded, want string) {
		t.Helper()

		if encoded == "" {
			t.Fatal("omitting registry auth would reuse old credentials on update")
		}

		auth, err := authconfig.Decode(encoded)
		if err != nil {
			t.Fatalf("decode registry auth: %v", err)
		}

		if auth.Username != want {
			t.Errorf("registry auth username = %q, want %q", auth.Username, want)
		}

		if want == "" && *auth != (registry.AuthConfig{}) {
			t.Error("expected empty registry credentials")
		}
	}

	for _, tc := range []struct {
		name          string
		image         string
		existingImage string
		missingHelper bool
		wantImage     string
		wantUser      string
		keepPlatforms bool
	}{
		{name: "create uses default image", wantImage: DefaultSwarmHelperImage},
		{name: "create uses override with registry auth", image: overrideImage, wantImage: overrideImage, wantUser: registryUser},
		{name: "update replaces legacy image", existingImage: legacyImage, wantImage: DefaultSwarmHelperImage},
		{name: "update uses override with registry auth", image: overrideImage, existingImage: legacyImage, wantImage: overrideImage, wantUser: registryUser},
		{name: "registry helper failure clears auth on update", image: overrideImage, existingImage: legacyImage, missingHelper: true, wantImage: overrideImage},
		{name: "same image keeps platforms", existingImage: DefaultSwarmHelperImage, wantImage: DefaultSwarmHelperImage, keepPlatforms: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			apiClient := &swarmJobSpecClient{}
			if tc.existingImage != "" {
				apiClient.existing = existingService(tc.existingImage)
			}

			err := RunSwarmJob(t.Context(), newCLI(apiClient, tc.missingHelper), swarm.DeployModeGlobalJob, tc.image, []string{"true"}, title)
			if !errors.Is(err, errSwarmJobSpecStop) {
				t.Fatalf("RunSwarmJob() error = %v, want %v", err, errSwarmJobSpecStop)
			}

			if apiClient.createOpts == nil {
				t.Fatal("ServiceCreate was not called")
			}

			if got := apiClient.createOpts.Spec.TaskTemplate.ContainerSpec.Image; got != tc.wantImage {
				t.Errorf("create image = %q, want %q", got, tc.wantImage)
			}

			requireAuthUser(t, apiClient.createOpts.EncodedRegistryAuth, tc.wantUser)

			if tc.existingImage == "" {
				return
			}

			if apiClient.updateOpts == nil {
				t.Fatal("ServiceUpdate was not called")
			}

			if got := apiClient.updateOpts.Spec.TaskTemplate.ContainerSpec.Image; got != tc.wantImage {
				t.Errorf("update image = %q, want %q", got, tc.wantImage)
			}

			requireAuthUser(t, apiClient.updateOpts.EncodedRegistryAuth, tc.wantUser)

			placement := apiClient.updateOpts.Spec.TaskTemplate.Placement
			if placement == nil || !slices.Equal(placement.Constraints, constraints) {
				t.Fatal("update did not keep existing placement constraints")
			}

			var wantPlatforms []swarmTypes.Platform
			if tc.keepPlatforms {
				wantPlatforms = platforms
			}

			if !slices.Equal(placement.Platforms, wantPlatforms) {
				t.Errorf("update platforms = %v, want %v", placement.Platforms, wantPlatforms)
			}
		})
	}
}
