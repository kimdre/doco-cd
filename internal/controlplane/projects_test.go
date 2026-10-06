package controlplane

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/lock"
)

func TestExecuteProjectActionWaitsForDeploymentLock(t *testing.T) {
	projectName := "project-action-lock-test"
	lockKey := lock.StackKey("", projectName)

	lock.LockStack(lockKey)

	locked := true

	t.Cleanup(func() {
		if locked {
			lock.UnlockStack(lockKey)
		}
	})

	actionStarted := make(chan struct{})
	actionDone := make(chan error, 1)
	operation := ProjectAction{
		projectName: projectName,
		action:      "stop",
		message:     "project stopped: " + projectName,
		lockKey:     lockKey,
		execute: func(context.Context, time.Duration, *slog.Logger) error {
			close(actionStarted)
			return nil
		},
	}

	go func() {
		_, err := ExecuteProjectAction(context.Background(), operation, DefaultProjectActionTimeout, slog.Default())
		actionDone <- err
	}()

	select {
	case <-actionStarted:
		t.Fatal("project action ran while a deployment held the stack lock")
	case <-time.After(50 * time.Millisecond):
	}

	lock.UnlockStack(lockKey)

	locked = false

	select {
	case err := <-actionDone:
		if err != nil {
			t.Fatalf("ExecuteProjectAction() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("project action did not run after the deployment released the stack lock")
	}
}
