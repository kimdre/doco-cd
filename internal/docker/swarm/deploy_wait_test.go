package swarm

import (
	"testing"

	swarmTypes "github.com/moby/moby/api/types/swarm"
)

func TestShouldWaitForService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		svc  deployedService
		want bool
	}{
		{
			name: "wait regular service",
			svc: deployedService{
				id: "svc-1",
			},
			want: true,
		},
		{
			name: "skip job mode service",
			svc: deployedService{
				id:        "svc-2",
				isJobMode: true,
			},
			want: false,
		},
		{
			name: "skip scheduled service",
			svc: deployedService{
				id:          "svc-3",
				isScheduled: true,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := shouldWaitForService(tt.svc); got != tt.want {
				t.Fatalf("shouldWaitForService() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsScheduledServiceSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec swarmTypes.ServiceSpec
		want bool
	}{
		{
			name: "missing container spec",
			spec: swarmTypes.ServiceSpec{},
			want: false,
		},
		{
			name: "missing label",
			spec: swarmTypes.ServiceSpec{
				TaskTemplate: swarmTypes.TaskSpec{
					ContainerSpec: &swarmTypes.ContainerSpec{Labels: map[string]string{}},
				},
			},
			want: false,
		},
		{
			name: "enabled true",
			spec: swarmTypes.ServiceSpec{
				TaskTemplate: swarmTypes.TaskSpec{
					ContainerSpec: &swarmTypes.ContainerSpec{Labels: map[string]string{scheduledJobEnabledLabel: "true"}},
				},
			},
			want: true,
		},
		{
			name: "enabled false",
			spec: swarmTypes.ServiceSpec{
				TaskTemplate: swarmTypes.TaskSpec{
					ContainerSpec: &swarmTypes.ContainerSpec{Labels: map[string]string{scheduledJobEnabledLabel: "false"}},
				},
			},
			want: false,
		},
		{
			name: "invalid bool",
			spec: swarmTypes.ServiceSpec{
				TaskTemplate: swarmTypes.TaskSpec{
					ContainerSpec: &swarmTypes.ContainerSpec{Labels: map[string]string{scheduledJobEnabledLabel: "yup"}},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isScheduledServiceSpec(tt.spec); got != tt.want {
				t.Fatalf("isScheduledServiceSpec() = %v, want %v", got, tt.want)
			}
		})
	}
}
