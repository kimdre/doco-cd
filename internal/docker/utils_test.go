package docker

import (
	"testing"

	swarmTypes "github.com/moby/moby/api/types/swarm"
)

func TestMergeSwarmLabels(t *testing.T) {
	service := swarmTypes.Service{
		Spec: swarmTypes.ServiceSpec{
			Annotations: swarmTypes.Annotations{
				Labels: map[string]string{
					"service.level": "from-service",
					"shared.key":    "service-value",
				},
			},
			TaskTemplate: swarmTypes.TaskSpec{
				ContainerSpec: &swarmTypes.ContainerSpec{
					Labels: map[string]string{
						"container.level": "from-container",
						"shared.key":      "container-value",
					},
				},
			},
		},
	}

	merged := mergeSwarmLabels(service)

	if merged["service.level"] != "from-service" {
		t.Error("expected service-level label to be present")
	}

	if merged["container.level"] != "from-container" {
		t.Error("expected container-level label to be present")
	}

	if merged["shared.key"] != "container-value" {
		t.Error("expected container labels to take precedence on collision")
	}
}
