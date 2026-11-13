package docker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	containerTypes "github.com/moby/moby/api/types/container"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

func TestJobTimeoutError(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("run job: %w", &JobTimeoutError{Timeout: 90 * time.Second})

	if !errors.Is(err, ErrScheduledJobTimeout) {
		t.Fatalf("errors.Is(%v, ErrScheduledJobTimeout) = false", err)
	}

	var timeoutErr *JobTimeoutError
	if !errors.As(err, &timeoutErr) || timeoutErr.Timeout != 90*time.Second {
		t.Fatalf("errors.As() did not return the timeout, got %+v", timeoutErr)
	}

	if got, want := timeoutErr.Error(), "exceeded timeout of 1m30s"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestContainerStopWaitTimeout(t *testing.T) {
	t.Parallel()

	seconds := func(v int) *int { return &v }

	for _, tc := range []struct {
		name   string
		config *containerTypes.Config
		want   time.Duration
	}{
		{name: "nil config", want: engineDefaultStopTimeout + jobStopWaitBuffer},
		{name: "engine default", config: &containerTypes.Config{}, want: engineDefaultStopTimeout + jobStopWaitBuffer},
		{name: "configured grace period", config: &containerTypes.Config{StopTimeout: seconds(45)}, want: 45*time.Second + jobStopWaitBuffer},
		{name: "zero grace period", config: &containerTypes.Config{StopTimeout: seconds(0)}, want: jobStopWaitBuffer},
		{name: "never killed", config: &containerTypes.Config{StopTimeout: seconds(-1)}, want: DefaultStopServicesTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := containerStopWaitTimeout(tc.config); got != tc.want {
				t.Fatalf("containerStopWaitTimeout() = %s, want %s", got, tc.want)
			}
		})
	}
}

type jobStopTestClient struct {
	client.APIClient

	mu          sync.Mutex
	inspectErr  error
	stopErr     error
	listErr     error
	listIDs     []string
	stopCalls   []string
	stopOptions []client.ContainerStopOptions
	stopCtxErr  error
}

func (c *jobStopTestClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if c.inspectErr != nil {
		return client.ContainerInspectResult{}, c.inspectErr
	}

	return client.ContainerInspectResult{Container: containerTypes.InspectResponse{Config: &containerTypes.Config{}}}, nil
}

func (c *jobStopTestClient) ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stopCalls = append(c.stopCalls, containerID)
	c.stopOptions = append(c.stopOptions, options)
	c.stopCtxErr = ctx.Err()

	return client.ContainerStopResult{}, c.stopErr
}

func (c *jobStopTestClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	if c.listErr != nil {
		return client.ContainerListResult{}, c.listErr
	}

	items := make([]containerTypes.Summary, 0, len(c.listIDs))
	for _, id := range c.listIDs {
		items = append(items, containerTypes.Summary{ID: id})
	}

	return client.ContainerListResult{Items: items}, nil
}

func TestStopScheduledContainer(t *testing.T) {
	t.Parallel()

	daemonErr := errors.New("daemon unavailable")

	for _, tc := range []struct {
		name       string
		inspectErr error
		stopErr    error
		wantStops  int
		wantErr    error
	}{
		{name: "graceful stop", wantStops: 1},
		{name: "container already removed", inspectErr: errdefs.ErrNotFound},
		{name: "container removed during stop", stopErr: errdefs.ErrNotFound, wantStops: 1},
		{name: "stop fails", stopErr: daemonErr, wantStops: 1, wantErr: daemonErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			apiClient := &jobStopTestClient{inspectErr: tc.inspectErr, stopErr: tc.stopErr}

			err := StopScheduledContainer(t.Context(), apiClient, "job-container")
			if tc.wantErr == nil && err != nil {
				t.Fatalf("StopScheduledContainer() error = %v", err)
			}

			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("StopScheduledContainer() error = %v, want %v", err, tc.wantErr)
			}

			if len(apiClient.stopCalls) != tc.wantStops {
				t.Fatalf("ContainerStop calls = %d, want %d", len(apiClient.stopCalls), tc.wantStops)
			}

			for _, options := range apiClient.stopOptions {
				if options.Signal != "" || options.Timeout != nil {
					t.Fatalf("ContainerStop options = %+v, want empty options so the container settings apply", options)
				}
			}
		})
	}
}

func TestStopScheduledContainerIgnoresCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	apiClient := &jobStopTestClient{}
	if err := StopScheduledContainer(ctx, apiClient, "job-container"); err != nil {
		t.Fatalf("StopScheduledContainer() error = %v", err)
	}

	if len(apiClient.stopCalls) != 1 || apiClient.stopCtxErr != nil {
		t.Fatalf("ContainerStop calls = %d, ctx error = %v; want one call with a live context", len(apiClient.stopCalls), apiClient.stopCtxErr)
	}
}

func TestStopOneOffContainer(t *testing.T) {
	t.Parallel()

	t.Run("no container", func(t *testing.T) {
		t.Parallel()

		apiClient := &jobStopTestClient{}
		if err := StopOneOffContainer(t.Context(), apiClient, "run-1"); err != nil {
			t.Fatalf("StopOneOffContainer() error = %v", err)
		}

		if len(apiClient.stopCalls) != 0 {
			t.Fatalf("ContainerStop calls = %d, want 0", len(apiClient.stopCalls))
		}
	})

	t.Run("retained container", func(t *testing.T) {
		t.Parallel()

		apiClient := &jobStopTestClient{listIDs: []string{"one-off-id"}}
		if err := StopOneOffContainer(t.Context(), apiClient, "run-1"); err != nil {
			t.Fatalf("StopOneOffContainer() error = %v", err)
		}

		if len(apiClient.stopCalls) != 1 || apiClient.stopCalls[0] != "one-off-id" {
			t.Fatalf("ContainerStop calls = %v, want [one-off-id]", apiClient.stopCalls)
		}
	})

	t.Run("lookup fails", func(t *testing.T) {
		t.Parallel()

		listErr := errors.New("daemon unavailable")

		apiClient := &jobStopTestClient{listErr: listErr}
		if err := StopOneOffContainer(t.Context(), apiClient, "run-1"); !errors.Is(err, listErr) {
			t.Fatalf("StopOneOffContainer() error = %v, want %v", err, listErr)
		}
	})
}

type swarmJobStopTestClient struct {
	client.APIClient

	mu          sync.Mutex
	inspectErr  error
	removeErr   error
	listErr     error
	taskListErr error
	services    []swarmTypes.Service
	listFilter  client.Filters
	tasks       []swarmTypes.Task
	removed     bool
	taskInspect func(taskID string, removed bool) (swarmTypes.Task, error)
}

func (c *swarmJobStopTestClient) ServiceList(_ context.Context, options client.ServiceListOptions) (client.ServiceListResult, error) {
	c.listFilter = options.Filters

	return client.ServiceListResult{Items: c.services}, c.listErr
}

func (c *swarmJobStopTestClient) ServiceInspect(context.Context, string, client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	if c.inspectErr != nil {
		return client.ServiceInspectResult{}, c.inspectErr
	}

	grace := time.Duration(0)

	return client.ServiceInspectResult{Service: swarmTypes.Service{
		ID: "service-id",
		Spec: swarmTypes.ServiceSpec{
			Annotations:  swarmTypes.Annotations{Name: "stack_backup-doco-job"},
			TaskTemplate: swarmTypes.TaskSpec{ContainerSpec: &swarmTypes.ContainerSpec{StopGracePeriod: &grace}},
		},
	}}, nil
}

func (c *swarmJobStopTestClient) TaskList(context.Context, client.TaskListOptions) (client.TaskListResult, error) {
	return client.TaskListResult{Items: c.tasks}, c.taskListErr
}

func (c *swarmJobStopTestClient) ServiceRemove(context.Context, string, client.ServiceRemoveOptions) (client.ServiceRemoveResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.removed = true

	return client.ServiceRemoveResult{}, c.removeErr
}

func (c *swarmJobStopTestClient) TaskInspect(_ context.Context, taskID string, _ client.TaskInspectOptions) (client.TaskInspectResult, error) {
	c.mu.Lock()
	removed := c.removed
	c.mu.Unlock()

	task, err := c.taskInspect(taskID, removed)

	return client.TaskInspectResult{Task: task}, err
}

func TestStopSwarmOneOffService(t *testing.T) {
	t.Parallel()

	runningTask := swarmTypes.Task{ID: "task-1", Status: swarmTypes.TaskStatus{State: swarmTypes.TaskStateRunning}}
	doneTask := swarmTypes.Task{ID: "task-0", Status: swarmTypes.TaskStatus{State: swarmTypes.TaskStateComplete}}
	removeErr := errors.New("daemon unavailable")

	for _, tc := range []struct {
		name        string
		inspectErr  error
		removeErr   error
		taskInspect func(taskID string, removed bool) (swarmTypes.Task, error)
		wantRemoved bool
		wantErr     error
	}{
		{
			name:       "service already removed",
			inspectErr: errdefs.ErrNotFound,
		},
		{
			name: "task shuts down",
			taskInspect: func(taskID string, removed bool) (swarmTypes.Task, error) {
				if taskID != runningTask.ID {
					return swarmTypes.Task{}, fmt.Errorf("unexpected task %s", taskID)
				}

				if !removed {
					return runningTask, nil
				}

				return swarmTypes.Task{ID: taskID, Status: swarmTypes.TaskStatus{State: swarmTypes.TaskStateShutdown}}, nil
			},
			wantRemoved: true,
		},
		{
			name: "task removed with service",
			taskInspect: func(string, bool) (swarmTypes.Task, error) {
				return swarmTypes.Task{}, errdefs.ErrNotFound
			},
			wantRemoved: true,
		},
		{
			name:        "remove fails",
			removeErr:   removeErr,
			wantRemoved: true,
			wantErr:     removeErr,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			apiClient := &swarmJobStopTestClient{
				inspectErr:  tc.inspectErr,
				removeErr:   tc.removeErr,
				tasks:       []swarmTypes.Task{doneTask, runningTask},
				taskInspect: tc.taskInspect,
			}

			err := StopSwarmOneOffService(t.Context(), swarmJobIdentityCLI{apiClient: apiClient}, "service-id")
			if tc.wantErr == nil && err != nil {
				t.Fatalf("StopSwarmOneOffService() error = %v", err)
			}

			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("StopSwarmOneOffService() error = %v, want %v", err, tc.wantErr)
			}

			if apiClient.removed != tc.wantRemoved {
				t.Fatalf("service removed = %v, want %v", apiClient.removed, tc.wantRemoved)
			}
		})
	}
}

func TestStopSwarmOneOffRun(t *testing.T) {
	t.Parallel()

	t.Run("removes service of run", func(t *testing.T) {
		t.Parallel()

		apiClient := &swarmJobStopTestClient{
			services: []swarmTypes.Service{{ID: "service-id"}},
			taskInspect: func(string, bool) (swarmTypes.Task, error) {
				return swarmTypes.Task{}, errdefs.ErrNotFound
			},
		}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if err := StopSwarmOneOffRun(ctx, swarmJobIdentityCLI{apiClient: apiClient}, "run-1"); err != nil {
			t.Fatalf("StopSwarmOneOffRun() error = %v", err)
		}

		if !apiClient.removed {
			t.Fatal("service was not removed")
		}

		if _, ok := apiClient.listFilter["label"][DocoCDJobLabels.JobRunID+"=run-1"]; !ok {
			t.Fatalf("service list filter = %v, want run ID label", apiClient.listFilter)
		}
	})

	t.Run("no service for run", func(t *testing.T) {
		t.Parallel()

		apiClient := &swarmJobStopTestClient{}
		if err := StopSwarmOneOffRun(t.Context(), swarmJobIdentityCLI{apiClient: apiClient}, "run-1"); err != nil {
			t.Fatalf("StopSwarmOneOffRun() error = %v", err)
		}

		if apiClient.removed {
			t.Fatal("service was removed without a match")
		}
	})

	t.Run("list fails", func(t *testing.T) {
		t.Parallel()

		listErr := errors.New("daemon unavailable")

		apiClient := &swarmJobStopTestClient{listErr: listErr}
		if err := StopSwarmOneOffRun(t.Context(), swarmJobIdentityCLI{apiClient: apiClient}, "run-1"); !errors.Is(err, listErr) {
			t.Fatalf("StopSwarmOneOffRun() error = %v, want %v", err, listErr)
		}
	})
}

func TestSwarmOneOffServiceActive(t *testing.T) {
	t.Parallel()

	runningTask := swarmTypes.Task{ID: "task-1", Status: swarmTypes.TaskStatus{State: swarmTypes.TaskStateRunning}}
	doneTask := swarmTypes.Task{ID: "task-0", Status: swarmTypes.TaskStatus{State: swarmTypes.TaskStateComplete}}
	listErr := errors.New("daemon unavailable")

	for _, tc := range []struct {
		name    string
		tasks   []swarmTypes.Task
		listErr error
		want    bool
	}{
		{name: "no tasks yet", want: true},
		{name: "task running", tasks: []swarmTypes.Task{doneTask, runningTask}, want: true},
		{name: "all tasks terminal", tasks: []swarmTypes.Task{doneTask}, want: false},
		{name: "list fails", listErr: listErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			apiClient := &swarmJobStopTestClient{tasks: tc.tasks, taskListErr: tc.listErr}

			got, err := SwarmOneOffServiceActive(t.Context(), swarmJobIdentityCLI{apiClient: apiClient}, "service-id")
			if tc.listErr != nil {
				if !errors.Is(err, tc.listErr) {
					t.Fatalf("SwarmOneOffServiceActive() error = %v, want %v", err, tc.listErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("SwarmOneOffServiceActive() error = %v", err)
			}

			if got != tc.want {
				t.Fatalf("SwarmOneOffServiceActive() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSwarmTaskIsTerminal(t *testing.T) {
	t.Parallel()

	for state, want := range map[swarmTypes.TaskState]bool{
		swarmTypes.TaskStateRunning:  false,
		swarmTypes.TaskStatePending:  false,
		swarmTypes.TaskStateShutdown: true,
		swarmTypes.TaskStateComplete: true,
		swarmTypes.TaskStateFailed:   true,
		swarmTypes.TaskStateRejected: true,
		swarmTypes.TaskStateOrphaned: true,
		swarmTypes.TaskStateRemove:   true,
	} {
		if got := swarmTaskIsTerminal(state); got != want {
			t.Errorf("swarmTaskIsTerminal(%s) = %v, want %v", state, got, want)
		}
	}
}
