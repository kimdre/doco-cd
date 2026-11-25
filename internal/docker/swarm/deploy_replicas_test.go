package swarm

import (
	"io"
	"strings"
	"testing"

	"github.com/docker/cli/cli/compose/convert"
	swarmTypes "github.com/moby/moby/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/docker/options"
)

func replicatedScheduledSpec(replicas *uint64) swarmTypes.ServiceSpec {
	return swarmTypes.ServiceSpec{
		Mode: swarmTypes.ServiceMode{
			Replicated: &swarmTypes.ReplicatedService{Replicas: replicas},
		},
		TaskTemplate: swarmTypes.TaskSpec{
			ContainerSpec: &swarmTypes.ContainerSpec{
				Labels: map[string]string{scheduledJobEnabledLabel: "true"},
			},
		},
	}
}

func TestApplyScheduledJobDeployReplicas_ReplicatedScheduled(t *testing.T) {
	t.Parallel()

	three := uint64(3)
	services := map[string]swarmTypes.ServiceSpec{
		"job": replicatedScheduledSpec(&three),
	}

	applyScheduledJobDeployReplicas(services)

	spec := services["job"]
	if spec.Mode.Replicated.Replicas == nil || *spec.Mode.Replicated.Replicas != 0 {
		t.Fatalf("expected scheduled service to be pinned to 0 replicas, got %v", spec.Mode.Replicated.Replicas)
	}

	if got := spec.TaskTemplate.ContainerSpec.Labels[scheduledJobRestartReplicasLabel]; got != "3" {
		t.Fatalf("expected intended replica count 3 to be recorded, got %q", got)
	}
}

func TestApplyScheduledJobDeployReplicas_DefaultsToOne(t *testing.T) {
	t.Parallel()

	services := map[string]swarmTypes.ServiceSpec{
		"job": replicatedScheduledSpec(nil),
	}

	applyScheduledJobDeployReplicas(services)

	spec := services["job"]
	if spec.Mode.Replicated.Replicas == nil || *spec.Mode.Replicated.Replicas != 0 {
		t.Fatalf("expected scheduled service to be pinned to 0 replicas")
	}

	if got := spec.TaskTemplate.ContainerSpec.Labels[scheduledJobRestartReplicasLabel]; got != "1" {
		t.Fatalf("expected default intended replica count 1, got %q", got)
	}
}

func TestApplyScheduledJobDeployReplicas_IgnoresNonScheduled(t *testing.T) {
	t.Parallel()

	two := uint64(2)
	services := map[string]swarmTypes.ServiceSpec{
		"api": {
			Mode: swarmTypes.ServiceMode{
				Replicated: &swarmTypes.ReplicatedService{Replicas: &two},
			},
			TaskTemplate: swarmTypes.TaskSpec{
				ContainerSpec: &swarmTypes.ContainerSpec{Labels: map[string]string{}},
			},
		},
	}

	applyScheduledJobDeployReplicas(services)

	spec := services["api"]
	if spec.Mode.Replicated.Replicas == nil || *spec.Mode.Replicated.Replicas != 2 {
		t.Fatalf("non-scheduled service replicas must not change, got %v", spec.Mode.Replicated.Replicas)
	}

	if _, ok := spec.TaskTemplate.ContainerSpec.Labels[scheduledJobRestartReplicasLabel]; ok {
		t.Fatalf("non-scheduled service must not receive the restart replicas label")
	}
}

func TestApplyScheduledJobDeployReplicas_IgnoresGlobalScheduled(t *testing.T) {
	t.Parallel()

	services := map[string]swarmTypes.ServiceSpec{
		"job": {
			Mode: swarmTypes.ServiceMode{
				Global: &swarmTypes.GlobalService{},
			},
			TaskTemplate: swarmTypes.TaskSpec{
				ContainerSpec: &swarmTypes.ContainerSpec{
					Labels: map[string]string{scheduledJobEnabledLabel: "true"},
				},
			},
		},
	}

	applyScheduledJobDeployReplicas(services)

	spec := services["job"]
	if spec.Mode.Global == nil {
		t.Fatalf("global scheduled service mode must be preserved")
	}

	if _, ok := spec.TaskTemplate.ContainerSpec.Labels[scheduledJobRestartReplicasLabel]; ok {
		t.Fatalf("global scheduled service cannot be scaled to 0 and must not be labeled")
	}
}

func replicatedSpec(replicas *uint64, labels map[string]string) swarmTypes.ServiceSpec {
	return swarmTypes.ServiceSpec{
		Mode: swarmTypes.ServiceMode{
			Replicated: &swarmTypes.ReplicatedService{Replicas: replicas},
		},
		TaskTemplate: swarmTypes.TaskSpec{
			ContainerSpec: &swarmTypes.ContainerSpec{Labels: labels},
		},
	}
}

func TestApplyLivePreservedReplicas(t *testing.T) {
	t.Parallel()

	one := uint64(1)
	five := uint64(5)

	for _, tt := range []struct {
		name     string
		desired  swarmTypes.ServiceSpec
		live     swarmTypes.ServiceSpec
		wantOK   bool
		wantLive bool
	}{
		{
			name:     "copies live replicas",
			desired:  replicatedSpec(&one, nil),
			live:     replicatedSpec(&five, nil),
			wantOK:   true,
			wantLive: true,
		},
		{
			name:    "live service without replica count applies Compose replicas",
			desired: replicatedSpec(&one, nil),
			live:    replicatedSpec(nil, nil),
		},
		{
			name:    "mode change from global applies Compose definition",
			desired: replicatedSpec(&one, nil),
			live: swarmTypes.ServiceSpec{
				Mode: swarmTypes.ServiceMode{Global: &swarmTypes.GlobalService{}},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			original := tt.desired.Mode.Replicated
			spec := tt.desired

			got, ok := applyLivePreservedReplicas(&spec, tt.live)
			if ok != tt.wantOK {
				t.Fatalf("applyLivePreservedReplicas() ok = %v, want %v", ok, tt.wantOK)
			}

			want := one
			if tt.wantLive {
				want = five
			}

			if ok && got != want {
				t.Fatalf("applyLivePreservedReplicas() replicas = %d, want %d", got, want)
			}

			if spec.Mode.Replicated == nil || spec.Mode.Replicated.Replicas == nil || *spec.Mode.Replicated.Replicas != want {
				t.Fatalf("spec replicas = %v, want %d", spec.Mode.Replicated, want)
			}

			if *original.Replicas != one {
				t.Fatalf("source spec replicas changed to %d", *original.Replicas)
			}

			if ok && spec.Mode.Replicated.Replicas == tt.live.Mode.Replicated.Replicas {
				t.Fatal("spec must not alias the live replica pointer")
			}
		})
	}
}

func TestApplyLivePreservedReplicas_IgnoresOtherModes(t *testing.T) {
	t.Parallel()

	five := uint64(5)

	for _, tt := range []struct {
		name string
		mode swarmTypes.ServiceMode
	}{
		{name: "global", mode: swarmTypes.ServiceMode{Global: &swarmTypes.GlobalService{}}},
		{name: "replicated job", mode: swarmTypes.ServiceMode{ReplicatedJob: &swarmTypes.ReplicatedJob{}}},
		{name: "global job", mode: swarmTypes.ServiceMode{GlobalJob: &swarmTypes.GlobalJob{}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spec := swarmTypes.ServiceSpec{Mode: tt.mode}

			if _, ok := applyLivePreservedReplicas(&spec, replicatedSpec(&five, nil)); ok {
				t.Fatal("non-replicated service must use the Compose definition")
			}

			if spec.Mode.Replicated != nil {
				t.Fatalf("service mode changed to replicated: %v", spec.Mode)
			}
		})
	}
}

func TestDeployServicesPreserveReplicas(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name        string
		label       string
		selfService string
		scheduled   bool
		existing    bool
		wantErr     bool
		want        uint64
	}{
		{name: "update keeps live replicas", label: "true", existing: true, want: 5},
		{name: "label value is trimmed and case insensitive", label: " TRUE ", existing: true, want: 5},
		{name: "label false uses Compose replicas", label: "false", existing: true, want: 1},
		{name: "missing label uses Compose replicas", existing: true, want: 1},
		{name: "self service uses Compose replicas", label: "true", existing: true, selfService: "stack_api", want: 1},
		{name: "invalid label ignored for self service", label: "invalid", existing: true, selfService: "stack_api", want: 1},
		{name: "invalid label ignored for scheduled job", label: "invalid", existing: true, scheduled: true, want: 1},
		{name: "create uses Compose replicas", label: "true", want: 1},
		{name: "invalid label rejects first deployment", label: "invalid", wantErr: true},
		{name: "invalid label rejects update", label: "invalid", existing: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			one := uint64(1)
			five := uint64(5)

			labels := map[string]string{}
			if tt.label != "" {
				labels[PreserveReplicasLabel] = tt.label
			}

			if tt.scheduled {
				labels[scheduledJobEnabledLabel] = "true"
			}

			spec := replicatedSpec(&one, labels)
			spec.Name = "stack_api"
			spec.TaskTemplate.ContainerSpec.Image = "example:latest"

			apiClient := &artifactRecoveryClient{}

			if tt.existing {
				live := replicatedSpec(&five, nil)
				live.Name = "stack_api"
				live.TaskTemplate.ContainerSpec.Image = "example:latest"
				apiClient.services = []swarmTypes.Service{{ID: "api-id", Spec: live}}
			}

			services := map[string]swarmTypes.ServiceSpec{"api": spec}
			opts := &options.Deploy{ResolveImage: ResolveImageNever, SelfService: tt.selfService}
			cli := testServiceIdentityCLI(apiClient, io.Discard, io.Discard)

			_, err := deployServices(t.Context(), cli, services, convert.NewNamespace("stack"), opts)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "service api: invalid "+PreserveReplicasLabel) {
					t.Fatalf("expected a service-specific label error, got %v", err)
				}

				if len(apiClient.creates) != 0 || len(apiClient.updates) != 0 {
					t.Fatal("invalid configuration must not create or update services")
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			var got *uint64

			switch {
			case len(apiClient.updates) == 1:
				got = apiClient.updates[0].Spec.Mode.Replicated.Replicas
			case len(apiClient.creates) == 1:
				got = apiClient.creates[0].Spec.Mode.Replicated.Replicas
			default:
				t.Fatalf("updates = %d, creates = %d, want one request", len(apiClient.updates), len(apiClient.creates))
			}

			if got == nil || *got != tt.want {
				t.Fatalf("submitted replicas = %v, want %d", got, tt.want)
			}

			if *services["api"].Mode.Replicated.Replicas != one {
				t.Fatal("deployServices changed the Compose service definition")
			}
		})
	}
}
