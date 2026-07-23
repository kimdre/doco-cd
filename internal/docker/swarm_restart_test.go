package docker

import (
	"testing"

	swarmTypes "github.com/moby/moby/api/types/swarm"
)

func TestScheduledRestartReplicas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec swarmTypes.ServiceSpec
		want uint64
	}{
		{
			name: "no container spec defaults to one",
			spec: swarmTypes.ServiceSpec{},
			want: 1,
		},
		{
			name: "missing label defaults to one",
			spec: swarmTypes.ServiceSpec{
				TaskTemplate: swarmTypes.TaskSpec{
					ContainerSpec: &swarmTypes.ContainerSpec{Labels: map[string]string{}},
				},
			},
			want: 1,
		},
		{
			name: "reads stored replica count",
			spec: swarmTypes.ServiceSpec{
				TaskTemplate: swarmTypes.TaskSpec{
					ContainerSpec: &swarmTypes.ContainerSpec{
						Labels: map[string]string{docoCDJobLabelNames.JobRestartReplicas: "4"},
					},
				},
			},
			want: 4,
		},
		{
			name: "invalid value defaults to one",
			spec: swarmTypes.ServiceSpec{
				TaskTemplate: swarmTypes.TaskSpec{
					ContainerSpec: &swarmTypes.ContainerSpec{
						Labels: map[string]string{docoCDJobLabelNames.JobRestartReplicas: "not-a-number"},
					},
				},
			},
			want: 1,
		},
		{
			name: "zero value defaults to one",
			spec: swarmTypes.ServiceSpec{
				TaskTemplate: swarmTypes.TaskSpec{
					ContainerSpec: &swarmTypes.ContainerSpec{
						Labels: map[string]string{docoCDJobLabelNames.JobRestartReplicas: "0"},
					},
				},
			},
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := scheduledRestartReplicas(tt.spec); got != tt.want {
				t.Fatalf("scheduledRestartReplicas() = %d, want %d", got, tt.want)
			}
		})
	}
}
