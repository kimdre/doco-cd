package stages

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

func acquireTestStack(t *testing.T, s *StageManager) *docker.LockedStack {
	t.Helper()

	stack, err := s.AcquireStack(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(stack.Release)

	return stack
}

func TestWaitForStackLockPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		trigger JobTrigger
		origin  syncwindow.Origin
		want    bool
	}{
		{name: "webhook waits", trigger: JobTriggerWebhook, origin: syncwindow.OriginAutomatic, want: true},
		{name: "manual webhook waits", trigger: JobTriggerWebhook, origin: syncwindow.OriginManual, want: true},
		{name: "poll skips", trigger: JobTriggerPoll, origin: syncwindow.OriginAutomatic},
		{name: "api poll skips", trigger: JobTriggerPoll, origin: syncwindow.OriginManual},
		{name: "reconciliation of a webhook deployment skips", trigger: JobTriggerWebhook, origin: syncwindow.OriginReconciliation},
		{name: "reconciliation of a poll deployment skips", trigger: JobTriggerPoll, origin: syncwindow.OriginReconciliation},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := &StageManager{
				DeployConfig: &deployConfig.Config{Name: "web"},
				JobTrigger:   tt.trigger,
				Origin:       tt.origin,
			}

			if got := s.waitForStackLock(); got != tt.want {
				t.Fatalf("waitForStackLock() = %v, want %v", got, tt.want)
			}

			opts := s.stackLockOptions()
			if opts.SkipIfBusy == tt.want {
				t.Fatalf("stackLockOptions().SkipIfBusy = %v, want %v", opts.SkipIfBusy, !tt.want)
			}

			if !strings.Contains(opts.Holder, "deployment of web") {
				t.Fatalf("stackLockOptions().Holder = %q, want the stack name", opts.Holder)
			}
		})
	}
}

func TestStackLockHolder(t *testing.T) {
	t.Parallel()

	s := &StageManager{
		DeployConfig: &deployConfig.Config{Name: "web"},
		JobTrigger:   JobTriggerPoll,
		JobID:        "job-1",
	}

	if got, want := s.stackLockHolder(), "deployment of web (job job-1, poll)"; got != want {
		t.Fatalf("stackLockHolder() = %q, want %q", got, want)
	}

	s.JobID = ""
	if got, want := s.stackLockHolder(), "deployment of web (poll)"; got != want {
		t.Fatalf("stackLockHolder() without job ID = %q, want %q", got, want)
	}
}

func TestAcquireStackSkipsLockedStack(t *testing.T) {
	t.Parallel()

	const (
		stackName   = "stack-lock-skip-test"
		contextName = "stack-lock-skip-context"
		holder      = "scheduled job backup (run 42)"
	)

	log := slog.New(slog.DiscardHandler)
	poll := &StageManager{
		Log:          log,
		DeployConfig: &deployConfig.Config{Name: stackName, Context: contextName},
		JobTrigger:   JobTriggerPoll,
	}

	otherContextKey := lock.StackKey("other-"+contextName, stackName)
	if ok, _ := lock.TryLockStack(otherContextKey, holder); !ok {
		t.Fatal("TryLockStack() on the other context failed")
	}

	t.Cleanup(func() { lock.UnlockStack(otherContextKey) })

	freeStack, err := poll.AcquireStack(t.Context())
	if err != nil {
		t.Fatalf("AcquireStack() with a lock on another context = %v, want nil", err)
	}

	freeStack.Release()

	key := lock.StackKey(contextName, stackName)
	if ok, _ := lock.TryLockStack(key, holder); !ok {
		t.Fatal("TryLockStack() failed")
	}

	t.Cleanup(func() { lock.UnlockStack(key) })

	lockedStack, err := poll.AcquireStack(t.Context())
	if lockedStack != nil {
		lockedStack.Release()
		t.Fatal("AcquireStack() took a busy lock")
	}

	if !errors.Is(err, ErrStackLocked) || !errors.Is(err, ErrSkipDeployment) {
		t.Fatalf("AcquireStack() = %v, want ErrStackLocked", err)
	}

	stack := NewSkippedStack(stackName, contextName, err)
	if stack.Reason != SkipReasonStackLocked || stack.Detail != holder {
		t.Fatalf("NewSkippedStack() = %+v, want reason %q and detail %q", stack, SkipReasonStackLocked, holder)
	}

	if got, locked := lock.StackLockHolder(key); !locked || got != holder {
		t.Fatalf("StackLockHolder() = %q, %v; the check changed the lock", got, locked)
	}
}
