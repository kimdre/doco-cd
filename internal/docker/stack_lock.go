package docker

import (
	"context"
	"log/slog"

	"github.com/kimdre/doco-cd/internal/lock"
)

// StackLockOptions controls how an operation takes the per-stack lock that
// keeps deployments and scheduled job runs of a stack apart.
type StackLockOptions struct {
	// Holder describes the operation in log and skip messages of other
	// operations that find the stack locked.
	Holder string
	// SkipIfBusy returns a *lock.StackBusyError at once when another operation
	// holds the stack lock. Otherwise the operation waits for the lock until
	// its context ends.
	SkipIfBusy bool
}

// LockedStack lets one deployment inspect, migrate, and deploy a stack under
// the same lock. Do not use it after calling Release.
type LockedStack struct {
	name    string
	key     string
	release func()
}

// Release releases the stack lock. Call it once.
func (s *LockedStack) Release() {
	s.release()
}

// AcquireStackLock takes the per-stack lock. It logs the current holder
// before waiting for a busy lock.
func AcquireStackLock(ctx context.Context, log *slog.Logger, contextName, stackName string, opts StackLockOptions) (*LockedStack, error) {
	if log == nil {
		log = slog.Default().With(slog.String("stack", stackName))
	}

	key := lock.StackKey(contextName, stackName)

	release, err := lock.AcquireStack(ctx, key, lock.AcquireStackOptions{
		Holder: opts.Holder,
		Wait:   !opts.SkipIfBusy,
		OnWait: func(holder string) {
			log.Info("waiting for stack lock", slog.String("lock_holder", holder))
		},
	})
	if err != nil {
		return nil, err
	}

	return &LockedStack{name: stackName, key: key, release: release}, nil
}
