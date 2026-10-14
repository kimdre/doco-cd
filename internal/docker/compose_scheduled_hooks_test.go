package docker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

type completedHookJobClient struct {
	client.APIClient

	restarted bool
	condition container.WaitCondition
	response  chan container.WaitResponse
	hookTried chan struct{}
}

type scheduledExitSubscriptionClient struct {
	client.APIClient

	waitCtx context.Context
	sent    chan struct{}
}

func (c *scheduledExitSubscriptionClient) ContainerWait(ctx context.Context, _ string, _ client.ContainerWaitOptions) client.ContainerWaitResult {
	c.waitCtx = ctx
	result := make(chan container.WaitResponse)

	go func() {
		result <- container.WaitResponse{}

		close(c.sent)
	}()

	return client.ContainerWaitResult{Result: result}
}

func TestComposeScheduledExitSubscriptionDrainsAbandonedResponse(t *testing.T) {
	t.Parallel()

	apiClient := &scheduledExitSubscriptionClient{sent: make(chan struct{})}

	_, cancel := subscribeComposeScheduledExit(t.Context(), apiClient, "backup-1", container.WaitConditionNextExit)
	defer cancel()

	select {
	case <-apiClient.sent:
	case <-time.After(time.Second):
		t.Fatal("abandoned wait response was not drained")
	}

	cancel()

	if !errors.Is(apiClient.waitCtx.Err(), context.Canceled) {
		t.Fatalf("wait context error = %v, want context.Canceled", apiClient.waitCtx.Err())
	}
}

func (c *completedHookJobClient) ContainerRestart(context.Context, string, client.ContainerRestartOptions) (client.ContainerRestartResult, error) {
	c.restarted = true

	return client.ContainerRestartResult{}, nil
}

func (*completedHookJobClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: container.InspectResponse{State: &container.State{Running: true}}}, nil
}

func (c *completedHookJobClient) ContainerWait(ctx context.Context, _ string, opts client.ContainerWaitOptions) client.ContainerWaitResult {
	c.condition = opts.Condition

	if c.response != nil {
		return client.ContainerWaitResult{Result: c.response}
	}

	result := make(chan container.WaitResponse, 1)
	errs := make(chan error, 1)

	if opts.Condition == container.WaitConditionNotRunning && c.restarted {
		result <- container.WaitResponse{StatusCode: 7}
	} else {
		go func() {
			<-ctx.Done()

			errs <- ctx.Err()
		}()
	}

	return client.ContainerWaitResult{Result: result, Error: errs}
}

func (c *completedHookJobClient) ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error) {
	if c.hookTried != nil {
		close(c.hookTried)
	}

	return client.ExecCreateResult{}, errdefs.ErrConflict
}

func TestRestartComposeScheduledContainerWaitsAfterFailedPostStart(t *testing.T) {
	t.Parallel()

	apiClient := &completedHookJobClient{
		response:  make(chan container.WaitResponse, 1),
		hookTried: make(chan struct{}),
	}
	// Release the fake wait even if an assertion fails.
	defer close(apiClient.response)

	svc := types.ServiceConfig{Name: "backup", PostStart: []types.ServiceHook{shellHook("true")}}
	done := make(chan error, 1)

	go func() {
		done <- restartComposeScheduledContainer(t.Context(), apiClient, "backup-1", svc, true)
	}()

	select {
	case <-apiClient.hookTried:
	case <-time.After(time.Second):
		t.Fatal("post_start hook was not attempted")
	}

	select {
	case err := <-done:
		t.Fatalf("returned before the job exited: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	apiClient.response <- container.WaitResponse{}

	select {
	case err := <-done:
		if !errors.Is(err, ErrComposeHookFailed) {
			t.Fatalf("restart error = %v, want the post_start failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("restart did not return after the job exited")
	}
}

func TestRestartComposeScheduledContainerFastExit(t *testing.T) {
	t.Parallel()

	for _, hooks := range []bool{false, true} {
		name := "without hooks"
		if hooks {
			name = "with hooks"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()

			apiClient := &completedHookJobClient{}

			svc := types.ServiceConfig{Name: "backup"}
			if hooks {
				svc.PostStart = []types.ServiceHook{shellHook("true")}
			}

			err := restartComposeScheduledContainer(ctx, apiClient, "backup-1", svc, true)
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("wait missed the completed job: %v", err)
			}

			var exitErr *ContainerExitError
			if errors.Is(err, ErrComposeHookFailed) != hooks || !errors.As(err, &exitErr) || exitErr.ExitCode != 7 {
				t.Fatalf("restart error = %v, want exit status 7 and hook failure=%v", err, hooks)
			}

			if !apiClient.restarted || apiClient.condition != container.WaitConditionNotRunning {
				t.Fatalf("restart = %v, wait condition = %q", apiClient.restarted, apiClient.condition)
			}
		})
	}
}

func TestComposeHookSourceLockCancellation(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"source GC", "deployment GC", "artifact"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			source := filepath.Join(root, "source")
			deployment := filepath.Join(root, "deployment")
			artifact := filepath.Join(deployment, "artifacts", "revision")
			ref := composeScheduledServiceRef{WorkingDir: filepath.Join(artifact, "deploy")}

			var (
				unlock func()
				err    error
			)

			switch kind {
			case "source GC":
				unlock, err = sourcecache.AcquireExclusiveGCPathLock(source)
			case "deployment GC":
				unlock, err = sourcecache.AcquireExclusiveGCPathLock(deployment)
			case "artifact":
				unlock = sourcecache.AcquirePathLock(artifact)
			}

			if err != nil {
				t.Fatal(err)
			}

			defer unlock()

			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()

			done := make(chan error, 1)

			go func() {
				release, err := lockScheduledSource(ctx, ref, root, source)
				if release != nil {
					release()
				}

				done <- err
			}()

			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("source lock error = %v, want context.DeadlineExceeded", err)
				}
			case <-time.After(time.Second):
				t.Fatal("source lock did not respect the load timeout")
			}

			if kind != "source GC" {
				releaseSource, acquired, err := sourcecache.TryAcquireExclusiveGCPathLock(source)
				if err != nil || !acquired {
					t.Fatalf("source GC lock leaked: acquired=%v, err=%v", acquired, err)
				}

				releaseSource()
			}
		})
	}
}
