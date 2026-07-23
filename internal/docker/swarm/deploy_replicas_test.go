package swarm

import (
	"testing"

	swarmTypes "github.com/moby/moby/api/types/swarm"
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
