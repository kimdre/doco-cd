package docker

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/platforms"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/selfupdate"
	"github.com/kimdre/doco-cd/internal/source/store"
)

type unchangedSelfClient struct {
	client.APIClient
	labels        map[string]string
	image         image.InspectResponse
	network       *network.Inspect
	inspectErr    error
	imageErr      error
	failManifest  bool
	listErr       error
	containersErr error
}

func (c unchangedSelfClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if c.inspectErr != nil {
		return client.ContainerInspectResult{}, c.inspectErr
	}

	return client.ContainerInspectResult{Container: container.InspectResponse{
		Config:     &container.Config{Labels: c.labels},
		HostConfig: &container.HostConfig{RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}},
	}}, nil
}

func (c unchangedSelfClient) ImageInspect(_ context.Context, _ string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if c.failManifest && len(opts) > 0 {
		return client.ImageInspectResult{}, errors.New("manifests unsupported")
	}

	if c.imageErr != nil {
		return client.ImageInspectResult{}, c.imageErr
	}

	return client.ImageInspectResult{InspectResponse: c.image}, nil
}

func (c unchangedSelfClient) NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error) {
	if c.listErr != nil {
		return client.NetworkListResult{}, c.listErr
	}

	if c.network == nil {
		return client.NetworkListResult{}, nil
	}

	return client.NetworkListResult{Items: []network.Summary{{Network: c.network.Network}}}, nil
}

func (c unchangedSelfClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	if c.containersErr != nil {
		return client.ContainerListResult{}, c.containersErr
	}

	return client.ContainerListResult{}, nil
}

func (c unchangedSelfClient) NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	return client.NetworkInspectResult{Network: *c.network}, nil
}

// countingInspectClient fails ContainerInspect on the third call.
// The unchanged check and the restart-policy check both inspect first.
type countingInspectClient struct {
	unchangedSelfClient
	calls int
	err   error
}

func (c *countingInspectClient) ContainerInspect(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	c.calls++
	if c.calls >= 3 {
		return client.ContainerInspectResult{}, c.err
	}

	return c.unchangedSelfClient.ContainerInspect(ctx, id, opts)
}

func TestSelfServiceUnchangedSkipsHandover(t *testing.T) {
	withSelfIdentity(t, "tools", "doco-cd")

	cfg := SelfUpdateConfig()
	cfg.Store = selfupdate.NewStore(t.TempDir())
	ConfigureSelfUpdate(cfg)

	project := selfUpdateTestProject("tools", "doco-cd")
	svc := project.Services["doco-cd"]
	svc.CustomLabels = types.Labels{api.ImageDigestLabel: "ignored", "cd.doco.commit": "aaa"}
	project.Services["doco-cd"] = svc

	hash, err := composeServiceHash(svc)
	if err != nil {
		t.Fatal(err)
	}

	digest := "sha256:running"
	apiClient := unchangedSelfClient{
		labels: map[string]string{
			api.ConfigHashLabel:  hash,
			api.ImageDigestLabel: digest,
		},
		image: image.InspectResponse{ID: digest},
	}
	target := &selfTarget{Service: "doco-cd", Project: project.Name}
	docker := selfApplyTestCli{apiClient: apiClient}
	services := []string{"doco-cd", "healthchecks"}

	unchanged, err := selfServiceUnchanged(t.Context(), apiClient, project, target, api.RecreateDiverged, nil)
	if err != nil || !unchanged {
		t.Fatalf("selfServiceUnchanged() = %v, %v; want skip", unchanged, err)
	}

	plan, skipped, err := planSelfHandover(
		t.Context(), docker, project, &deploy.Config{Name: "tools"}, target, api.RecreateDiverged, services,
		&SelfDeployInput{SourceType: "git"}, composeDeployOptions{},
	)
	if err != nil || !skipped || plan.Step != nil {
		t.Fatalf("planSelfHandover() skipped=%v step=%v err=%v; want no handover", skipped, plan.Step != nil, err)
	}

	kept, createServices := applySelfPlan(project, services, "doco-cd", plan)
	if _, ok := kept.Services["doco-cd"]; !ok || !slices.Contains(createServices, "doco-cd") {
		t.Fatalf("self service left the compose create: project=%v services=%v", kept.ServiceNames(), createServices)
	}

	labeled := project.Services["doco-cd"]
	labeled.CustomLabels = types.Labels{api.ImageDigestLabel: "ignored", "cd.doco.commit": "bbb"}

	relabeled, err := composeServiceHash(labeled)
	if err != nil {
		t.Fatal(err)
	}

	if relabeled != hash {
		t.Fatalf("commit label changed the config hash: %s -> %s", hash, relabeled)
	}

	project.Services["doco-cd"] = labeled

	unchanged, err = selfServiceUnchanged(t.Context(), apiClient, project, target, api.RecreateDiverged, nil)
	if err != nil || !unchanged {
		t.Fatalf("commit label caused a handover: unchanged=%v err=%v", unchanged, err)
	}
}

func TestChangedSelfServiceStillHandsOver(t *testing.T) {
	withSelfIdentity(t, "tools", "doco-cd")

	cfg := SelfUpdateConfig()
	cfg.Store = selfupdate.NewStore(t.TempDir())
	ConfigureSelfUpdate(cfg)

	project := selfUpdateTestProject("tools", "doco-cd")
	original := project.Services["doco-cd"]

	hash, err := composeServiceHash(original)
	if err != nil {
		t.Fatal(err)
	}

	digest := "sha256:running"
	apiClient := unchangedSelfClient{
		labels: map[string]string{
			api.ConfigHashLabel:  hash,
			api.ImageDigestLabel: digest,
		},
		image: image.InspectResponse{ID: digest},
	}
	target := &selfTarget{Service: "doco-cd", Project: project.Name}
	docker := selfApplyTestCli{apiClient: apiClient}
	services := []string{"doco-cd", "healthchecks"}

	env := "changed"

	for _, tc := range []struct {
		name   string
		mutate func(types.ServiceConfig) types.ServiceConfig
	}{
		{name: "image", mutate: func(svc types.ServiceConfig) types.ServiceConfig {
			svc.Image = "ghcr.io/kimdre/doco-cd:next"
			return svc
		}},
		{name: "environment", mutate: func(svc types.ServiceConfig) types.ServiceConfig {
			svc.Environment = types.MappingWithEquals{"DOCO_CD_FOO": &env}
			return svc
		}},
		{name: "mount", mutate: func(svc types.ServiceConfig) types.ServiceConfig {
			svc.Volumes = []types.ServiceVolumeConfig{{Type: types.VolumeTypeBind, Source: "/data", Target: "/data"}}
			return svc
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := *project
			changed.Services = types.Services{
				"doco-cd": tc.mutate(original),
				"other":   project.Services["other"],
			}

			unchanged, err := selfServiceUnchanged(t.Context(), apiClient, &changed, target, api.RecreateDiverged, nil)
			if err != nil || unchanged {
				t.Fatalf("selfServiceUnchanged() = %v, %v; want handover", unchanged, err)
			}

			plan, skipped, err := planSelfHandover(
				t.Context(), docker, &changed, &deploy.Config{Name: "tools"}, target, api.RecreateDiverged, services,
				&SelfDeployInput{SourceType: "git"}, composeDeployOptions{},
			)
			if err != nil || skipped || plan.Step == nil || plan.DeferToApplier {
				t.Fatalf("plan = skipped %v defer %v step %v err %v; want scale-out handover", skipped, plan.DeferToApplier, plan.Step != nil, err)
			}

			if slices.Contains(plan.Services, "doco-cd") || !slices.Contains(plan.Services, "healthchecks") {
				t.Fatalf("handover create services = %v; want the self service removed", plan.Services)
			}

			strategy, _, err := selectSelfStrategy(t.Context(), docker, &changed, target, "git", nil)
			if err != nil || strategy != selfupdate.StrategyScaleOut {
				t.Fatalf("strategy = %s, %v; want scale_out", strategy, err)
			}

			removed, createServices := applySelfPlan(&changed, services, "doco-cd", plan)
			if _, ok := removed.Services["doco-cd"]; ok || slices.Contains(createServices, "doco-cd") {
				t.Fatalf("compose create still includes doco-cd: services=%v", createServices)
			}
		})
	}
}

func TestSelfServiceHashMissStillHandsOver(t *testing.T) {
	withSelfIdentity(t, "tools", "doco-cd")

	cfg := SelfUpdateConfig()
	cfg.Store = selfupdate.NewStore(t.TempDir())
	ConfigureSelfUpdate(cfg)

	project := selfUpdateTestProject("tools", "doco-cd")
	svc := project.Services["doco-cd"]

	hash, err := composeServiceHash(svc)
	if err != nil {
		t.Fatal(err)
	}

	digest := "sha256:running"
	target := &selfTarget{Service: "doco-cd", Project: project.Name}

	t.Run("missing config hash", func(t *testing.T) {
		apiClient := unchangedSelfClient{
			labels: map[string]string{api.ImageDigestLabel: digest},
			image:  image.InspectResponse{ID: digest},
		}

		unchanged, err := selfServiceUnchanged(t.Context(), apiClient, project, target, api.RecreateDiverged, nil)
		if err != nil || unchanged {
			t.Fatalf("selfServiceUnchanged() = %v, %v; want handover", unchanged, err)
		}
	})

	t.Run("image digest changed", func(t *testing.T) {
		apiClient := unchangedSelfClient{
			labels: map[string]string{
				api.ConfigHashLabel:  hash,
				api.ImageDigestLabel: digest,
			},
			image: image.InspectResponse{ID: "sha256:pulled"},
		}

		unchanged, err := selfServiceUnchanged(t.Context(), apiClient, project, target, api.RecreateDiverged, nil)
		if err != nil || unchanged {
			t.Fatalf("selfServiceUnchanged() = %v, %v; want handover", unchanged, err)
		}
	})

	t.Run("missing image digest", func(t *testing.T) {
		apiClient := unchangedSelfClient{labels: map[string]string{api.ConfigHashLabel: hash}}

		unchanged, err := selfServiceUnchanged(t.Context(), apiClient, project, target, api.RecreateDiverged, nil)
		if err != nil || unchanged {
			t.Fatalf("selfServiceUnchanged() = %v, %v; want handover", unchanged, err)
		}
	})

	two := 2
	scaled := svc
	scaled.Scale = &two
	project.Services["doco-cd"] = scaled

	t.Run("scale is not 1", func(t *testing.T) {
		apiClient := unchangedSelfClient{
			labels: map[string]string{
				api.ConfigHashLabel:  hash,
				api.ImageDigestLabel: digest,
			},
			image: image.InspectResponse{ID: digest},
		}

		unchanged, err := selfServiceUnchanged(t.Context(), apiClient, project, target, api.RecreateDiverged, nil)
		if err != nil || unchanged {
			t.Fatalf("selfServiceUnchanged() = %v, %v; want handover", unchanged, err)
		}
	})
}

func TestSelfServiceNetworkDriftAndForceStillHandOver(t *testing.T) {
	withSelfIdentity(t, "tools", "doco-cd")

	cfg := SelfUpdateConfig()
	cfg.Store = selfupdate.NewStore(t.TempDir())
	ConfigureSelfUpdate(cfg)

	project := selfUpdateTestProject("tools", "doco-cd")
	svc := project.Services["doco-cd"]
	svc.Networks = map[string]*types.ServiceNetworkConfig{"backend": {}}
	project.Services["doco-cd"] = svc

	netCfg := types.NetworkConfig{Name: "tools_backend"}
	project.Networks = types.Networks{"backend": netCfg}

	hash, err := composeServiceHash(project.Services["doco-cd"])
	if err != nil {
		t.Fatal(err)
	}

	netHash, err := composeNetworkHash(&netCfg)
	if err != nil {
		t.Fatal(err)
	}

	digest := "sha256:running"

	apiClient := unchangedSelfClient{
		labels: map[string]string{
			api.ConfigHashLabel:  hash,
			api.ImageDigestLabel: digest,
		},
		image: image.InspectResponse{ID: digest},
		network: &network.Inspect{
			ID: "network-id", Name: netCfg.Name,
			Labels: map[string]string{
				api.ProjectLabel: "tools", api.NetworkLabel: "backend", api.ConfigHashLabel: "old",
			},
		},
	}
	if apiClient.network.Labels[api.ConfigHashLabel] == netHash {
		t.Fatal("fixture network hash matches; drift would not be detected")
	}

	target := &selfTarget{Service: "doco-cd", Project: project.Name}
	docker := selfApplyTestCli{apiClient: apiClient}

	unchanged, err := selfServiceUnchanged(t.Context(), apiClient, project, target, api.RecreateDiverged, nil)
	if err != nil || unchanged {
		t.Fatalf("network drift selfServiceUnchanged() = %v, %v; want handover", unchanged, err)
	}

	plan, skipped, err := planSelfHandover(
		t.Context(), docker, project, &deploy.Config{Name: "tools"}, target, api.RecreateDiverged, []string{"doco-cd", "healthchecks"},
		&SelfDeployInput{SourceType: "git"}, composeDeployOptions{},
	)
	if err != nil || skipped || !plan.DeferToApplier || plan.Step == nil {
		t.Fatalf("network drift plan skipped=%v defer=%v err=%v", skipped, plan.DeferToApplier, err)
	}

	for _, tc := range []struct {
		name     string
		mode     string
		services []string
	}{
		{name: "project force", mode: api.RecreateForce},
		{name: "service force", mode: api.RecreateForce, services: []string{"doco-cd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forced := unchangedSelfClient{
				labels: apiClient.labels,
				image:  apiClient.image,
			}

			unchanged, err := selfServiceUnchanged(t.Context(), forced, project, target, tc.mode, tc.services)
			if err != nil || unchanged {
				t.Fatalf("selfServiceUnchanged() = %v, %v; want handover", unchanged, err)
			}
		})
	}
}

func TestComposeImageContentDigestUsesRunnableManifest(t *testing.T) {
	t.Parallel()

	host := platforms.DefaultSpec()

	other := specs.Platform{OS: "linux", Architecture: "s390x"}
	if host.Architecture == other.Architecture {
		other.Architecture = "amd64"
	}

	inspect := image.InspectResponse{
		ID: "sha256:index",
		Manifests: []image.ManifestSummary{
			{ID: "sha256:attestation", Kind: image.ManifestKindAttestation, Available: true},
			{ID: "sha256:other", Kind: image.ManifestKindImage, Available: true, ImageData: &image.ImageProperties{Platform: other}},
			{ID: "sha256:host", Kind: image.ManifestKindImage, Available: true, ImageData: &image.ImageProperties{Platform: host}},
		},
	}

	got, err := composeImageContentDigest(inspect, "")
	if err != nil || got != "sha256:host" {
		t.Fatalf("composeImageContentDigest() = %s, %v; want the host manifest", got, err)
	}
}

func TestSelfServiceUnchangedClosedFailures(t *testing.T) {
	withSelfIdentity(t, "tools", "doco-cd")

	project := selfUpdateTestProject("tools", "doco-cd")
	target := &selfTarget{Service: "doco-cd", Project: project.Name}

	hash, err := composeServiceHash(project.Services["doco-cd"])
	if err != nil {
		t.Fatal(err)
	}

	digest := "sha256:running"
	labels := map[string]string{
		api.ConfigHashLabel:  hash,
		api.ImageDigestLabel: digest,
	}
	matched := unchangedSelfClient{labels: labels, image: image.InspectResponse{ID: digest}}

	t.Run("image volume", func(t *testing.T) {
		changed := *project
		svc := project.Services["doco-cd"]
		svc.Volumes = []types.ServiceVolumeConfig{{Type: types.VolumeTypeImage, Source: "alpine", Target: "/data"}}
		changed.Services = types.Services{"doco-cd": svc}

		unchanged, err := selfServiceUnchanged(t.Context(), unchangedSelfClient{}, &changed, target, api.RecreateDiverged, nil)
		if err != nil || unchanged {
			t.Fatalf("selfServiceUnchanged() = %v, %v; want handover", unchanged, err)
		}
	})

	t.Run("image volume digest", func(t *testing.T) {
		apiClient := unchangedSelfClient{labels: map[string]string{
			api.ConfigHashLabel:        hash,
			api.ImageVolumeDigestLabel: "sha256:volume",
		}}

		unchanged, err := selfServiceUnchanged(t.Context(), apiClient, project, target, api.RecreateDiverged, nil)
		if err != nil || unchanged {
			t.Fatalf("selfServiceUnchanged() = %v, %v; want handover", unchanged, err)
		}
	})

	t.Run("inspect error", func(t *testing.T) {
		apiClient := unchangedSelfClient{inspectErr: errors.New("inspect failed")}

		unchanged, err := selfServiceUnchanged(t.Context(), apiClient, project, target, api.RecreateDiverged, nil)
		if err == nil || unchanged {
			t.Fatalf("selfServiceUnchanged() = %v, %v; want inspect error", unchanged, err)
		}
	})

	t.Run("network list error", func(t *testing.T) {
		changed := *project
		svc := project.Services["doco-cd"]
		svc.Networks = map[string]*types.ServiceNetworkConfig{"backend": {}}
		changed.Services = types.Services{"doco-cd": svc}
		changed.Networks = types.Networks{"backend": {Name: "tools_backend"}}

		apiClient := unchangedSelfClient{labels: labels, listErr: errors.New("list failed")}

		unchanged, err := selfServiceUnchanged(t.Context(), apiClient, &changed, target, api.RecreateDiverged, nil)
		if err == nil || unchanged {
			t.Fatalf("selfServiceUnchanged() = %v, %v; want network error", unchanged, err)
		}
	})

	t.Run("image inspect error", func(t *testing.T) {
		apiClient := matched
		apiClient.imageErr = errors.New("image missing")

		unchanged, err := selfServiceUnchanged(t.Context(), apiClient, project, target, api.RecreateDiverged, nil)
		if err != nil || unchanged {
			t.Fatalf("selfServiceUnchanged() = %v, %v; want handover", unchanged, err)
		}
	})

	t.Run("canceled image inspect", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		apiClient := matched
		apiClient.imageErr = errors.New("image missing")

		unchanged, err := selfServiceUnchanged(ctx, apiClient, project, target, api.RecreateDiverged, nil)
		if err == nil || unchanged {
			t.Fatalf("selfServiceUnchanged() = %v, %v; want context error", unchanged, err)
		}
	})

	t.Run("manifest inspect falls back", func(t *testing.T) {
		apiClient := matched
		apiClient.failManifest = true

		unchanged, err := selfServiceUnchanged(t.Context(), apiClient, project, target, api.RecreateDiverged, nil)
		if err != nil || !unchanged {
			t.Fatalf("selfServiceUnchanged() = %v, %v; want skip", unchanged, err)
		}
	})

	t.Run("force of another service", func(t *testing.T) {
		unchanged, err := selfServiceUnchanged(t.Context(), matched, project, target, api.RecreateForce, []string{"healthchecks"})
		if err != nil || !unchanged {
			t.Fatalf("selfServiceUnchanged() = %v, %v; want skip", unchanged, err)
		}
	})

	t.Run("bad platform", func(t *testing.T) {
		svc := project.Services["doco-cd"]
		svc.Platform = "not a platform"

		unchanged, err := selfImageDigestMatches(t.Context(), matched, project.Name, svc, digest)
		if err != nil || unchanged {
			t.Fatalf("selfImageDigestMatches() = %v, %v; want handover", unchanged, err)
		}
	})

	t.Run("canceled bad platform", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		svc := project.Services["doco-cd"]
		svc.Platform = "not a platform"

		unchanged, err := selfImageDigestMatches(ctx, matched, project.Name, svc, digest)
		if err == nil || unchanged {
			t.Fatalf("selfImageDigestMatches() = %v, %v; want context error", unchanged, err)
		}
	})
}

func TestComposeImageContentDigestBranches(t *testing.T) {
	t.Parallel()

	t.Run("invalid platform", func(t *testing.T) {
		t.Parallel()

		if _, err := composeImageContentDigest(image.InspectResponse{ID: "sha256:plain"}, "not a platform"); err == nil {
			t.Fatal("composeImageContentDigest() error = nil; want a platform parse error")
		}
	})

	t.Run("no manifests", func(t *testing.T) {
		t.Parallel()

		got, err := composeImageContentDigest(image.InspectResponse{ID: "sha256:plain"}, "")
		if err != nil || got != "sha256:plain" {
			t.Fatalf("composeImageContentDigest() = %s, %v; want the image id", got, err)
		}
	})

	t.Run("single manifest", func(t *testing.T) {
		t.Parallel()

		got, err := composeImageContentDigest(image.InspectResponse{
			ID: "sha256:index", Architecture: "amd64", Os: "linux",
			Manifests: []image.ManifestSummary{{
				ID: "sha256:only", Kind: image.ManifestKindImage, Available: true,
			}},
		}, "")
		if err != nil || got != "sha256:only" {
			t.Fatalf("composeImageContentDigest() = %s, %v; want the only manifest", got, err)
		}
	})

	t.Run("unmatched manifest", func(t *testing.T) {
		t.Parallel()

		got, err := composeImageContentDigest(image.InspectResponse{
			ID: "sha256:index",
			Manifests: []image.ManifestSummary{{
				ID: "sha256:other", Kind: image.ManifestKindImage, Available: true,
				ImageData: &image.ImageProperties{Platform: specs.Platform{OS: "linux", Architecture: "s390x"}},
			}},
		}, "linux/amd64")
		if err != nil || got != "sha256:other" {
			t.Fatalf("composeImageContentDigest() = %s, %v; want the only manifest", got, err)
		}
	})

	t.Run("no platform match", func(t *testing.T) {
		t.Parallel()

		got, err := composeImageContentDigest(image.InspectResponse{
			ID: "sha256:index",
			Manifests: []image.ManifestSummary{
				{ID: "sha256:one", Kind: image.ManifestKindImage, Available: true, ImageData: &image.ImageProperties{Platform: specs.Platform{OS: "linux", Architecture: "s390x"}}},
				{ID: "sha256:two", Kind: image.ManifestKindImage, Available: true, ImageData: &image.ImageProperties{Platform: specs.Platform{OS: "linux", Architecture: "ppc64le"}}},
			},
		}, "linux/amd64")
		if err != nil || got != "sha256:index" {
			t.Fatalf("composeImageContentDigest() = %s, %v; want the index id", got, err)
		}
	})
}

func TestComposeServiceHashIgnoresDeployReplicas(t *testing.T) {
	t.Parallel()

	one := 1
	two := 2
	base := types.ServiceConfig{Name: "doco-cd", Image: "app", Deploy: &types.DeployConfig{Replicas: &one}}
	changed := base
	changed.Deploy = &types.DeployConfig{Replicas: &two}

	left, err := composeServiceHash(base)
	if err != nil {
		t.Fatal(err)
	}

	right, err := composeServiceHash(changed)
	if err != nil {
		t.Fatal(err)
	}

	if left != right {
		t.Fatalf("replica change moved the config hash: %s -> %s", left, right)
	}
}

func TestPinRootKeyOrder(t *testing.T) {
	t.Parallel()

	got, err := pinRootKeyOrder([]byte(`{"zzz":1,"image":"app"}`), []string{"image"})
	if err != nil || string(got) != `{"image":"app","zzz":1}` {
		t.Fatalf("pinRootKeyOrder() = %s, %v", got, err)
	}

	if _, err = pinRootKeyOrder([]byte(`[1]`), nil); err == nil {
		t.Fatal("pinRootKeyOrder() error = nil; want invalid JSON")
	}
}

func TestPlanSelfHandoverContinuesWhenPinFails(t *testing.T) {
	withSelfIdentity(t, "tools", "doco-cd")

	cfg := SelfUpdateConfig()
	cfg.Store = selfupdate.NewStore(t.TempDir())
	ConfigureSelfUpdate(cfg)

	project := selfUpdateTestProject("tools", "doco-cd")

	hash, err := composeServiceHash(project.Services["doco-cd"])
	if err != nil {
		t.Fatal(err)
	}

	digest := "sha256:running"
	root := filepath.Join(t.TempDir(), store.ArtifactsSubdir, "rev")

	if err = os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	apiClient := unchangedSelfClient{
		labels: map[string]string{
			api.ConfigHashLabel:  hash,
			api.ImageDigestLabel: digest,
		},
		image:         image.InspectResponse{ID: digest},
		containersErr: errors.New("list failed"),
	}

	plan, skipped, err := planSelfHandover(
		t.Context(), selfApplyTestCli{apiClient: apiClient}, project, &deploy.Config{Name: "tools"},
		&selfTarget{Service: "doco-cd", Project: project.Name}, api.RecreateDiverged, nil,
		&SelfDeployInput{SourceType: "git"}, composeDeployOptions{ArtifactRoot: root},
	)
	if err != nil || !skipped || plan.Step != nil {
		t.Fatalf("planSelfHandover() skipped=%v step=%v err=%v; want skip after pin failure", skipped, plan.Step != nil, err)
	}
}

func TestRecheckSelfSkip(t *testing.T) {
	withSelfIdentity(t, "tools", "doco-cd")

	cfg := SelfUpdateConfig()
	cfg.Store = selfupdate.NewStore(t.TempDir())
	ConfigureSelfUpdate(cfg)

	project := selfUpdateTestProject("tools", "doco-cd")

	hash, err := composeServiceHash(project.Services["doco-cd"])
	if err != nil {
		t.Fatal(err)
	}

	digest := "sha256:running"
	apiClient := unchangedSelfClient{
		labels: map[string]string{
			api.ConfigHashLabel:  hash,
			api.ImageDigestLabel: digest,
		},
		image: image.InspectResponse{ID: digest},
	}
	target := &selfTarget{Service: "doco-cd", Project: project.Name}
	docker := selfApplyTestCli{apiClient: apiClient}
	dc := &deploy.Config{Name: "tools"}
	services := []string{"doco-cd", "healthchecks"}
	self := &SelfDeployInput{SourceType: "git"}

	t.Run("handover already chosen", func(t *testing.T) {
		plan := selfUpdatePlan{Services: []string{"healthchecks"}}
		broken := selfApplyTestCli{apiClient: unchangedSelfClient{inspectErr: errors.New("inspect failed")}}

		got, err := recheckSelfSkip(t.Context(), broken, project, dc, target, api.RecreateDiverged, services, self, plan, false)
		if err != nil || !slices.Equal(got.Services, plan.Services) {
			t.Fatalf("recheckSelfSkip() services=%v err=%v", got.Services, err)
		}
	})

	t.Run("still unchanged", func(t *testing.T) {
		got, err := recheckSelfSkip(t.Context(), docker, project, dc, target, api.RecreateDiverged, services, self, selfUpdatePlan{}, true)
		if err != nil || got.Step != nil {
			t.Fatalf("recheckSelfSkip() step=%v err=%v; want the service left in place", got.Step != nil, err)
		}
	})

	t.Run("inspect error", func(t *testing.T) {
		broken := selfApplyTestCli{apiClient: unchangedSelfClient{inspectErr: errors.New("inspect failed")}}

		_, err := recheckSelfSkip(t.Context(), broken, project, dc, target, api.RecreateDiverged, services, self, selfUpdatePlan{}, true)
		if err == nil {
			t.Fatal("recheckSelfSkip() error = nil; want the inspect error")
		}
	})

	t.Run("service now needs a handover", func(t *testing.T) {
		changed := *project
		svc := project.Services["doco-cd"]
		svc.Image = "ghcr.io/kimdre/doco-cd:next"
		changed.Services = types.Services{"doco-cd": svc, "other": project.Services["other"]}

		got, err := recheckSelfSkip(t.Context(), docker, &changed, dc, target, api.RecreateDiverged, services, self, selfUpdatePlan{}, true)
		if err != nil || got.Step == nil || got.DeferToApplier || !slices.Equal(got.Services, []string{"healthchecks"}) {
			t.Fatalf("recheckSelfSkip() services=%v defer=%v step=%v err=%v", got.Services, got.DeferToApplier, got.Step != nil, err)
		}
	})

	t.Run("plan returns the inspect error", func(t *testing.T) {
		broken := selfApplyTestCli{apiClient: unchangedSelfClient{inspectErr: errors.New("inspect failed")}}

		_, skipped, err := planSelfHandover(t.Context(), broken, project, dc, target, api.RecreateDiverged, services, self, composeDeployOptions{})
		if err == nil || skipped {
			t.Fatalf("planSelfHandover() skipped=%v err=%v; want the inspect error", skipped, err)
		}
	})

	t.Run("network drift starts the handover", func(t *testing.T) {
		drifted := *project
		svc := project.Services["doco-cd"]
		svc.Networks = map[string]*types.ServiceNetworkConfig{"backend": {}}
		drifted.Services = types.Services{"doco-cd": svc, "other": project.Services["other"]}
		netCfg := types.NetworkConfig{Name: "tools_backend"}
		drifted.Networks = types.Networks{"backend": netCfg}
		inspectErr := errors.New("inspect failed")
		apiClient := &countingInspectClient{
			unchangedSelfClient: unchangedSelfClient{
				labels: map[string]string{api.ConfigHashLabel: "present"},
				network: &network.Inspect{
					ID: "network-id", Name: netCfg.Name,
					Labels: map[string]string{
						api.ProjectLabel: "tools", api.NetworkLabel: "backend", api.ConfigHashLabel: "old",
					},
				},
			},
			err: inspectErr,
		}

		got, err := recheckSelfSkip(
			t.Context(), selfApplyTestCli{apiClient: apiClient}, &drifted, dc, target,
			api.RecreateDiverged, services, self, selfUpdatePlan{}, true,
		)

		if !errors.Is(err, inspectErr) || !got.DeferToApplier {
			t.Fatalf("recheckSelfSkip() defer=%v err=%v; want the drift handover to start", got.DeferToApplier, err)
		}
	})
}

func TestGuardSelfUpdateRefusals(t *testing.T) {
	previous := SelfUpdateConfig()

	t.Cleanup(func() { ConfigureSelfUpdate(previous) })

	project := selfUpdateTestProject("tools", "doco-cd")
	target := &selfTarget{Service: "doco-cd", Project: project.Name}
	docker := selfApplyTestCli{apiClient: unchangedSelfClient{}}
	dc := &deploy.Config{Name: "tools"}

	t.Run("disabled", func(t *testing.T) {
		ConfigureSelfUpdate(SelfUpdateOptions{Enabled: false})

		_, err := prepareSelfUpdate(t.Context(), docker, project, dc, target, nil, nil)

		if !errors.Is(err, selfupdate.ErrDisabled) {
			t.Fatalf("prepareSelfUpdate() error = %v; want self-update disabled", err)
		}
	})

	t.Run("journal unreadable", func(t *testing.T) {
		mount := t.TempDir()
		if err := os.WriteFile(filepath.Join(mount, "self-update"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		ConfigureSelfUpdate(SelfUpdateOptions{Enabled: true, Store: selfupdate.NewStore(mount)})

		_, err := prepareSelfUpdate(t.Context(), docker, project, dc, target, nil, nil)

		if err == nil {
			t.Fatal("prepareSelfUpdate() error = nil; want the journal read error")
		}
	})

	t.Run("handover in progress", func(t *testing.T) {
		store := selfupdate.NewStore(t.TempDir())
		if err := store.Create(&selfupdate.Record{
			ID: "5056a901-3c03-4478-91d4-7a20aab30e8e", State: selfupdate.StateStarted,
			Strategy: selfupdate.StrategyScaleOut, Stack: "tools", Service: "doco-cd",
		}); err != nil {
			t.Fatal(err)
		}

		ConfigureSelfUpdate(SelfUpdateOptions{Enabled: true, Store: store})

		stopped := false
		_, err := prepareSelfUpdate(t.Context(), docker, project, dc, target, nil, &SelfDeployInput{
			StopPhaseReports: func() { stopped = true },
		})

		if !errors.Is(err, selfupdate.ErrHandover) || !stopped {
			t.Fatalf("prepareSelfUpdate() stopped=%v err=%v; want the active handover", stopped, err)
		}
	})
}

func TestComposeServiceHashRejectsNaN(t *testing.T) {
	svc := types.ServiceConfig{Name: "doco-cd", CPUPercent: float32(math.NaN())}

	if _, err := composeServiceHash(svc); err == nil {
		t.Fatal("composeServiceHash() error = nil; want the NaN rejected")
	}

	withSelfIdentity(t, "tools", "doco-cd")

	project := selfUpdateTestProject("tools", "doco-cd")
	project.Services["doco-cd"] = svc
	unchanged, err := selfServiceUnchanged(
		t.Context(),
		unchangedSelfClient{labels: map[string]string{api.ConfigHashLabel: "present"}},
		project, &selfTarget{Service: "doco-cd", Project: project.Name}, api.RecreateDiverged, nil,
	)

	if err == nil || unchanged {
		t.Fatalf("selfServiceUnchanged() = %v, %v; want the hash error", unchanged, err)
	}
}

func TestSelfUpdateLogger(t *testing.T) {
	t.Parallel()

	if selfUpdateLogger(nil) == nil {
		t.Fatal("selfUpdateLogger(nil) = nil")
	}

	log := slog.New(slog.DiscardHandler)
	if selfUpdateLogger(&SelfDeployInput{Log: log}) != log {
		t.Fatal("selfUpdateLogger() did not keep the deploy logger")
	}
}
