package swarm

import (
	"strings"
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

func TestIsRollbackUpdateState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state swarmTypes.UpdateState
		want  bool
	}{
		{name: "rollback started", state: swarmTypes.UpdateStateRollbackStarted, want: true},
		{name: "rollback paused", state: swarmTypes.UpdateStateRollbackPaused, want: true},
		{name: "rollback completed", state: swarmTypes.UpdateStateRollbackCompleted, want: true},
		{name: "completed", state: swarmTypes.UpdateStateCompleted, want: false},
		{name: "updating", state: swarmTypes.UpdateStateUpdating, want: false},
		{name: "paused", state: swarmTypes.UpdateStatePaused, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isRollbackUpdateState(tt.state); got != tt.want {
				t.Fatalf("isRollbackUpdateState(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestIsTerminalNonRollbackUpdateState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state swarmTypes.UpdateState
		want  bool
	}{
		{name: "completed", state: swarmTypes.UpdateStateCompleted, want: true},
		{name: "paused", state: swarmTypes.UpdateStatePaused, want: true},
		{name: "updating", state: swarmTypes.UpdateStateUpdating, want: false},
		{name: "rollback started", state: swarmTypes.UpdateStateRollbackStarted, want: false},
		{name: "rollback paused", state: swarmTypes.UpdateStateRollbackPaused, want: false},
		{name: "rollback completed", state: swarmTypes.UpdateStateRollbackCompleted, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isTerminalNonRollbackUpdateState(tt.state); got != tt.want {
				t.Fatalf("isTerminalNonRollbackUpdateState(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestRollbackUpdateStatusError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		serviceID   string
		serviceName string
		status      *swarmTypes.UpdateStatus
		wantErr     bool
		wantParts   []string
	}{
		{
			name:        "nil status",
			serviceID:   "svc-id",
			serviceName: "stack_api",
			status:      nil,
			wantErr:     false,
		},
		{
			name:        "non rollback state",
			serviceID:   "svc-id",
			serviceName: "stack_api",
			status: &swarmTypes.UpdateStatus{
				State:   swarmTypes.UpdateStateCompleted,
				Message: "update completed",
			},
			wantErr: false,
		},
		{
			name:        "rollback uses service name and message",
			serviceID:   "svc-id",
			serviceName: "stack_api",
			status: &swarmTypes.UpdateStatus{
				State:   swarmTypes.UpdateStateRollbackCompleted,
				Message: "rollback completed",
			},
			wantErr:   true,
			wantParts: []string{"stack_api", string(swarmTypes.UpdateStateRollbackCompleted), "rollback completed"},
		},
		{
			name:        "rollback falls back to service id",
			serviceID:   "svc-id",
			serviceName: "   ",
			status: &swarmTypes.UpdateStatus{
				State: swarmTypes.UpdateStateRollbackStarted,
			},
			wantErr:   true,
			wantParts: []string{"svc-id", string(swarmTypes.UpdateStateRollbackStarted)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := rollbackUpdateStatusError(tt.serviceID, tt.serviceName, tt.status)
			if (err != nil) != tt.wantErr {
				t.Fatalf("rollbackUpdateStatusError() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err == nil {
				return
			}

			for _, wantPart := range tt.wantParts {
				if !strings.Contains(err.Error(), wantPart) {
					t.Fatalf("rollbackUpdateStatusError() error = %q, missing %q", err.Error(), wantPart)
				}
			}
		})
	}
}
