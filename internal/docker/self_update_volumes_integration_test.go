package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/selfupdate"
	internaltest "github.com/kimdre/doco-cd/internal/test"
)

// selfVolumeSafetyYAML builds a stack with independently changing networks
// and volumes for gated Docker integration tests.
func selfVolumeSafetyYAML(networkGeneration, volumeGeneration string) string {
	return fmt.Sprintf(`
services:
  app:
    image: alpine:3.22
    command: ["sleep", "600"]
    restart: unless-stopped
    volumes: [data:/data]
    networks: [backend]
volumes:
  data:
    labels:
      generation: "%s"
networks:
  backend:
    labels:
      generation: "%s"
`, volumeGeneration, networkGeneration)
}

type volumeChangesBeforeReducedCreateClient struct {
	client.APIClient
	name   string
	checks int
}

// VolumeList simulates a volume changing after preflight but before Create.
func (c *volumeChangesBeforeReducedCreateClient) VolumeList(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error) {
	result, err := c.APIClient.VolumeList(ctx, options)
	if err != nil {
		return result, err
	}

	c.checks++
	if c.checks >= 2 {
		for i := range result.Items {
			if result.Items[i].Name == c.name {
				result.Items[i].Labels = maps.Clone(result.Items[i].Labels)
				result.Items[i].Labels[api.ConfigHashLabel] = "changed-after-preflight"
			}
		}
	}

	return result, nil
}

// TestSelfUpdateIntegration_RejectsLateVolumeChangeBeforeReducedCreate checks
// validation immediately before creating the reduced self stack.
func TestSelfUpdateIntegration_RejectsLateVolumeChangeBeforeReducedCreate(t *testing.T) {
	requireSelfUpdateIntegrationGate(t)

	ctx := t.Context()
	stackName := internaltest.ConvertTestName(t.Name())
	stack := internaltest.ComposeUp(ctx, t,
		internaltest.WithName(stackName),
		internaltest.WithYAML(selfVolumeSafetyYAML("old", "old")))
	oldID := stack.ServiceContainerID(ctx, t, "app")
	project := loadSelfUpdateProject(ctx, t, stackName, selfVolumeSafetyYAML("old", "old"))
	app := project.Services["app"]
	app.PullPolicy = types.PullPolicyNever
	project.Services["app"] = app

	withSelfIdentity(t, stackName, "app")

	opts := SelfUpdateConfig()
	opts.Identity.ContainerID = oldID
	opts.Store = selfupdate.NewStore(t.TempDir())
	ConfigureSelfUpdate(opts)

	fake := &volumeChangesBeforeReducedCreateClient{APIClient: stack.Client, name: stackName + "_data"}

	err := deployCompose(ctx, selfApplyTestCli{Cli: stack.DockerCli, apiClient: fake},
		project, &deploy.Config{Name: stackName}, "", nil, nil, nil, &SelfDeployInput{SourceType: "git"})
	if !errors.Is(err, selfupdate.ErrUnsupported) || fake.checks < 2 {
		t.Fatalf("late volume change before reduced Create = %v after %d checks; want refusal", err, fake.checks)
	}

	existing, inspectErr := stack.Client.ContainerInspect(ctx, oldID, client.ContainerInspectOptions{})
	if inspectErr != nil || existing.Container.State == nil || !existing.Container.State.Running {
		t.Fatalf("predecessor changed before reduced Create: %v", inspectErr)
	}
}

// TestSelfUpdateIntegration_RejectsVolumeRecreationWithoutNetworkDrift checks
// that unchanged networks do not bypass volume safety checks.
func TestSelfUpdateIntegration_RejectsVolumeRecreationWithoutNetworkDrift(t *testing.T) {
	requireSelfUpdateIntegrationGate(t)

	ctx := t.Context()
	stackName := internaltest.ConvertTestName(t.Name())
	stack := internaltest.ComposeUp(ctx, t,
		internaltest.WithName(stackName),
		internaltest.WithYAML(selfVolumeSafetyYAML("old", "old")))
	oldID := stack.ServiceContainerID(ctx, t, "app")

	oldVolume, err := stack.Client.VolumeInspect(ctx, stackName+"_data", client.VolumeInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	nextYAML := strings.Replace(selfVolumeSafetyYAML("old", "new"), "alpine:3.22", "alpine:3.21", 1)
	project := loadSelfUpdateProject(ctx, t, stackName, nextYAML)

	drift, err := networkDrift(ctx, stack.Client, project, project.Services["app"])
	if err != nil || drift {
		t.Fatalf("network drift = %t/%v; want false", drift, err)
	}

	record := selfupdate.Record{
		State: selfupdate.StateStaged, Stack: stackName, Service: "app",
		Predecessor: selfupdate.ContainerRef{ID: oldID},
	}

	target := &selfTarget{Project: stackName, Service: "app"}
	if err := selfUpdateScaleOut(ctx, stack.DockerCli, project, &deploy.Config{}, target, &record, slog.Default()); !errors.Is(err, selfupdate.ErrUnsupported) {
		t.Errorf("scale-out Create with changed volume = %v; want refusal", err)
	}

	if err := applySelfService(ctx, stack.Client, nil, project, record, nil, slog.Default()); !errors.Is(err, selfupdate.ErrUnsupported) {
		t.Errorf("applier Create with changed volume = %v; want refusal", err)
	}

	after, err := stack.Client.VolumeInspect(ctx, stackName+"_data", client.VolumeInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if after.Volume.Labels["generation"] != "old" ||
		after.Volume.Labels[api.ConfigHashLabel] != oldVolume.Volume.Labels[api.ConfigHashLabel] {
		t.Fatalf("volume changed despite rejected self-update: %+v", after.Volume.Labels)
	}

	existing, err := stack.Client.ContainerInspect(ctx, oldID, client.ContainerInspectOptions{})
	if err != nil || existing.Container.State == nil || !existing.Container.State.Running {
		t.Fatalf("predecessor stopped by volume validation: %v", err)
	}
}

// TestSelfUpdateIntegration_RejectsVolumeRecreationWithNetworkDrift checks
// volume safety before the applier recreates project networks.
func TestSelfUpdateIntegration_RejectsVolumeRecreationWithNetworkDrift(t *testing.T) {
	requireSelfUpdateIntegrationGate(t)

	ctx := t.Context()
	stackName := internaltest.ConvertTestName(t.Name())
	stack := internaltest.ComposeUp(ctx, t,
		internaltest.WithName(stackName),
		internaltest.WithYAML(selfVolumeSafetyYAML("old", "old")))
	oldID := stack.ServiceContainerID(ctx, t, "app")

	oldVolume, err := stack.Client.VolumeInspect(ctx, stackName+"_data", client.VolumeInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	oldProject := loadSelfUpdateProject(ctx, t, stackName, selfVolumeSafetyYAML("old", "old"))
	if err := validateSelfUpdateVolumes(ctx, stack.Client, oldProject); err != nil {
		t.Fatalf("unchanged Compose volume refused: %v", err)
	}

	project := loadSelfUpdateProject(ctx, t, stackName, selfVolumeSafetyYAML("new", "new"))

	drift, err := networkDrift(ctx, stack.Client, project, project.Services["app"])
	if err != nil || !drift {
		t.Fatalf("network drift = %t/%v; want true", drift, err)
	}

	if err = validateSelfUpdateVolumes(ctx, stack.Client, project); !errors.Is(err, selfupdate.ErrUnsupported) {
		t.Fatalf("volume change before network drift = %v; want explicit refusal", err)
	}

	after, err := stack.Client.VolumeInspect(ctx, stackName+"_data", client.VolumeInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if after.Volume.Labels["generation"] != "old" ||
		after.Volume.Labels[api.ConfigHashLabel] != oldVolume.Volume.Labels[api.ConfigHashLabel] {
		t.Fatalf("volume was changed despite preflight rejection: %+v", after.Volume.Labels)
	}

	existing, err := stack.Client.ContainerInspect(ctx, oldID, client.ContainerInspectOptions{})
	if err != nil || existing.Container.State == nil || !existing.Container.State.Running {
		t.Fatalf("old service stopped by volume validation: %v", err)
	}
}
