package swarm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	swarmTypes "github.com/moby/moby/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

func TestSelfUpdateHandoverError(t *testing.T) {
	t.Parallel()

	self := deployedService{id: "self-service-id", name: "doco-cd_app"}
	canceled := fmt.Errorf("deploy: %w", context.Canceled)
	failure := errors.New("service update failed")
	tests := []struct {
		name          string
		selfService   string
		deployed      []deployedService
		err           error
		activeContext bool
		want          bool
	}{
		{name: "accepted self update interrupted", selfService: self.name, deployed: []deployedService{self}, err: canceled, want: true},
		{name: "operation canceled without shutdown", selfService: self.name, deployed: []deployedService{self}, err: canceled, activeContext: true},
		{name: "success", selfService: self.name, deployed: []deployedService{self}},
		{name: "self update not submitted", selfService: self.name, err: canceled},
		{name: "other service updated", selfService: self.name, deployed: []deployedService{{name: "doco-cd_worker"}}, err: canceled},
		{name: "not a self stack", deployed: []deployedService{self}, err: canceled},
		{name: "timeout", selfService: self.name, deployed: []deployedService{self}, err: context.DeadlineExceeded},
		{name: "service failure", selfService: self.name, deployed: []deployedService{self}, err: failure},
		{name: "joined failure", selfService: self.name, deployed: []deployedService{self}, err: errors.Join(canceled, failure)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if !tt.activeContext {
				cancel()
			}

			got := selfUpdateHandoverError(ctx, tt.err, tt.selfService, tt.deployed)
			if errors.Is(got, selfupdate.ErrHandover) != tt.want {
				t.Fatalf("selfUpdateHandoverError() = %v, want handover %v", got, tt.want)
			}

			if tt.want {
				if !errors.Is(got, context.Canceled) {
					t.Fatal("handover lost its cancellation cause")
				}
			} else if got != tt.err {
				t.Fatalf("selfUpdateHandoverError() changed non-handover error to %v", got)
			}
		})
	}
}

func TestWaitOnServicesPreservesFailureBeforeCancellation(t *testing.T) {
	t.Parallel()

	failure := errors.New("rollback paused")
	services := []deployedService{{id: "failed", name: "stack_worker"}, {id: "canceled", name: "stack_app"}}

	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			t.Parallel()

			order := services
			if reverse {
				order = []deployedService{services[1], services[0]}
			}

			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			err := waitOnServices(ctx, order, func(_ context.Context, service deployedService) error {
				if service.id == "failed" {
					return failure
				}

				return context.Canceled
			})
			if !errors.Is(err, failure) || errors.Is(err, context.Canceled) {
				t.Fatalf("waitOnServices() = %v, want failure without cancellation", err)
			}

			if errors.Is(selfUpdateHandoverError(ctx, err, services[1].name, services), selfupdate.ErrHandover) {
				t.Fatal("a service failure was turned into a self-update handover")
			}
		})
	}
}

func TestWaitOnServicesWithTimeout(t *testing.T) {
	t.Parallel()

	const timeout = 10 * time.Millisecond

	err := waitOnServicesWith(t.Context(), []deployedService{{id: "svc-1", name: "stack_api"}}, timeout, func(ctx context.Context, _ deployedService) error {
		<-ctx.Done()

		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitOnServicesWith() error = %v, want context deadline exceeded", err)
	}

	if !strings.Contains(err.Error(), "timed out after 10ms waiting for swarm services to converge") ||
		!strings.Contains(err.Error(), "stack_api (id=svc-1)") {
		t.Fatalf("waitOnServicesWith() error = %q, missing timeout context", err)
	}
}

func TestWaitOnServicesWithParentCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := waitOnServicesWith(ctx, []deployedService{{id: "svc-1", name: "stack_api"}}, time.Minute, func(ctx context.Context, _ deployedService) error {
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitOnServicesWith() error = %v, want context canceled", err)
	}

	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("waitOnServicesWith() error = %q, parent cancellation reported as timeout", err)
	}
}

func TestWaitOnServicesIncludesEveryServiceIdentity(t *testing.T) {
	t.Parallel()

	firstErr := errors.New("first update failed")
	secondErr := errors.New("second update failed")
	services := []deployedService{
		{id: "service-id-one", name: "stack_api"},
		{id: "service-id-two", name: "stack_worker"},
	}

	err := waitOnServices(t.Context(), services, func(_ context.Context, service deployedService) error {
		if service.id == services[0].id {
			return firstErr
		}

		return secondErr
	})
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("waitOnServices() = %v, want both underlying errors", err)
	}

	for _, service := range services {
		if !strings.Contains(err.Error(), ServiceIdentity(service.name, service.id)) {
			t.Fatalf("waitOnServices() = %v, missing %s", err, service.name)
		}
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
		name      string
		status    *swarmTypes.UpdateStatus
		wantErr   bool
		wantParts []string
	}{
		{name: "nil status"},
		{
			name: "non rollback state",
			status: &swarmTypes.UpdateStatus{
				State:   swarmTypes.UpdateStateCompleted,
				Message: "update completed",
			},
		},
		{
			name: "rollback includes state and message",
			status: &swarmTypes.UpdateStatus{
				State:   swarmTypes.UpdateStateRollbackCompleted,
				Message: "rollback completed",
			},
			wantErr:   true,
			wantParts: []string{string(swarmTypes.UpdateStateRollbackCompleted), "rollback completed"},
		},
		{
			name: "rollback without message",
			status: &swarmTypes.UpdateStatus{
				State: swarmTypes.UpdateStateRollbackStarted,
			},
			wantErr:   true,
			wantParts: []string{string(swarmTypes.UpdateStateRollbackStarted)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := rollbackUpdateStatusError(tt.status)
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
