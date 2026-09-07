package swarm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	swarmTypes "github.com/moby/moby/api/types/swarm"
)

func TestWaitOnServicesWithTimeout(t *testing.T) {
	t.Parallel()

	const timeout = 10 * time.Millisecond

	err := waitOnServicesWith(t.Context(), []string{"svc-1"}, timeout, func(ctx context.Context, _ string) error {
		<-ctx.Done()

		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitOnServicesWith() error = %v, want context deadline exceeded", err)
	}

	if !strings.Contains(err.Error(), "timed out after 10ms waiting for swarm services to converge") {
		t.Fatalf("waitOnServicesWith() error = %q, missing timeout context", err)
	}
}

func TestWaitOnServicesWithParentCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := waitOnServicesWith(ctx, []string{"svc-1"}, time.Minute, func(ctx context.Context, _ string) error {
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitOnServicesWith() error = %v, want context canceled", err)
	}

	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("waitOnServicesWith() error = %q, parent cancellation reported as timeout", err)
	}
}

func TestJobTaskFailureFromTasks(t *testing.T) {
	t.Parallel()

	iteration := uint64(7)
	otherIteration := uint64(6)
	tests := []struct {
		name  string
		tasks []swarmTypes.Task
		want  string
	}{
		{
			name: "current iteration failed task includes task details",
			tasks: []swarmTypes.Task{
				{
					ID:           "failed-task",
					JobIteration: &swarmTypes.Version{Index: iteration},
					Status: swarmTypes.TaskStatus{
						State: swarmTypes.TaskStateFailed,
						Err:   "process exited unexpectedly",
						ContainerStatus: &swarmTypes.ContainerStatus{
							ExitCode: 7,
						},
					},
				},
			},
			want: "job task failed-task ended in failed (exit code 7): process exited unexpectedly",
		},
		{
			name: "previous iteration failure is ignored",
			tasks: []swarmTypes.Task{
				{
					ID:           "old-failed-task",
					JobIteration: &swarmTypes.Version{Index: otherIteration},
					Status:       swarmTypes.TaskStatus{State: swarmTypes.TaskStateFailed},
				},
				{
					ID:           "current-complete-task",
					JobIteration: &swarmTypes.Version{Index: iteration},
					Status:       swarmTypes.TaskStatus{State: swarmTypes.TaskStateComplete},
				},
			},
		},
		{
			name: "current iteration rejected task uses message",
			tasks: []swarmTypes.Task{
				{
					ID:           "rejected-task",
					JobIteration: &swarmTypes.Version{Index: iteration},
					Status: swarmTypes.TaskStatus{
						State:   swarmTypes.TaskStateRejected,
						Message: "no suitable node",
					},
				},
			},
			want: "job task rejected-task ended in rejected: no suitable node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := jobTaskFailureFromTasks(tt.tasks, iteration)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("jobTaskFailureFromTasks() error = %v, want nil", err)
				}

				return
			}

			if err == nil || err.Error() != tt.want {
				t.Fatalf("jobTaskFailureFromTasks() error = %v, want %q", err, tt.want)
			}
		})
	}
}

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
