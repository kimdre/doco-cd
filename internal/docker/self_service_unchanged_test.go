package docker

import (
	"context"
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
)

type unchangedSelfClient struct {
	client.APIClient
	labels  map[string]string
	image   image.InspectResponse
	network *network.Inspect
}

func (c unchangedSelfClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: container.InspectResponse{
		Config:     &container.Config{Labels: c.labels},
		HostConfig: &container.HostConfig{RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}},
	}}, nil
}

func (c unchangedSelfClient) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	return client.ImageInspectResult{InspectResponse: c.image}, nil
}

func (c unchangedSelfClient) NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error) {
	if c.network == nil {
		return client.NetworkListResult{}, nil
	}

	return client.NetworkListResult{Items: []network.Summary{{Network: c.network.Network}}}, nil
}

func (c unchangedSelfClient) NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	return client.NetworkInspectResult{Network: *c.network}, nil
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
