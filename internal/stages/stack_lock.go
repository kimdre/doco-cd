package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

// waitForStackLock reports whether the deployment waits for a stack lock that
// another operation holds. Webhook deployments wait, because nothing repeats
// them. Polls, including polls triggered through the API, MCP, or a signal, and
// reconciliation redeploys skip the stack, because the next poll or Docker
// event tries it again. A poll that waits would stop all later polls of its
// configuration.
func (s *StageManager) waitForStackLock() bool {
	return s.JobTrigger == JobTriggerWebhook && s.Origin != syncwindow.OriginReconciliation
}

// stackLockHolder describes this deployment as the stack lock holder.
func (s *StageManager) stackLockHolder() string {
	holder := "deployment of " + s.DeployConfig.Name

	if s.JobID != "" {
		return fmt.Sprintf("%s (job %s, %s)", holder, s.JobID, s.JobTrigger)
	}

	return fmt.Sprintf("%s (%s)", holder, s.JobTrigger)
}

// stackLockOptions returns how docker operations of this deployment take the
// per-stack lock.
func (s *StageManager) stackLockOptions() docker.StackLockOptions {
	return docker.StackLockOptions{
		Holder:     s.stackLockHolder(),
		SkipIfBusy: !s.waitForStackLock(),
	}
}

// AcquireStack takes the deployment's stack lock before admission to either
// concurrency limiter. A waiting webhook must not use a slot needed by other stacks.
func (s *StageManager) AcquireStack(ctx context.Context) (*docker.LockedStack, error) {
	stack, err := docker.AcquireStackLock(ctx, s.Log, s.DeployConfig.Context, s.DeployConfig.Name, s.stackLockOptions())
	if busy, ok := errors.AsType[*lock.StackBusyError](err); ok {
		s.Log.Info("stack is locked by another operation, skipping deployment",
			slog.String("lock_holder", busy.Holder),
		)

		return nil, StackLocked(busy.Holder)
	}

	if err != nil {
		return nil, fmt.Errorf("acquire stack lock: %w", err)
	}

	return stack, nil
}
