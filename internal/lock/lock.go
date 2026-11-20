package lock

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// RepoLock represents a lock for a specific repository.
type RepoLock struct {
	mu      sync.Mutex
	locked  bool
	holder  string
	waiters []*repoLockWaiter
}

type repoLockWaiter struct {
	jobID   string
	granted chan struct{}
}

// TryLock attempts to acquire the lock for the given jobID.
// It returns true if the lock was successfully acquired.
func (l *RepoLock) TryLock(jobID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.locked {
		return false
	}

	l.locked = true
	l.holder = jobID

	return true
}

// Unlock releases the lock and hands it to the next queued waiter, if any.
// It panics when the lock is not held, mirroring sync.Mutex misuse semantics:
// a stray Unlock would otherwise silently grant the lock to a waiter while
// the real holder is still running.
func (l *RepoLock) Unlock() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.locked {
		panic("lock: Unlock of unlocked RepoLock")
	}

	if len(l.waiters) == 0 {
		l.locked = false
		l.holder = ""

		return
	}

	waiter := l.waiters[0]
	l.waiters = l.waiters[1:]
	l.holder = waiter.jobID
	close(waiter.granted)
}

// Lock acquires the lock, blocking until it is available.
func (l *RepoLock) Lock() {
	l.LockContext(context.Background(), "")
}

// LockContext acquires the lock in waiter order or returns false when ctx is cancelled.
func (l *RepoLock) LockContext(ctx context.Context, jobID string) bool {
	if ctx.Err() != nil {
		return false
	}

	l.mu.Lock()
	if !l.locked {
		l.locked = true
		l.holder = jobID
		l.mu.Unlock()

		return true
	}

	waiter := &repoLockWaiter{jobID: jobID, granted: make(chan struct{})}
	l.waiters = append(l.waiters, waiter)
	l.mu.Unlock()

	select {
	case <-waiter.granted:
		return true
	case <-ctx.Done():
		if l.removeQueuedWaiter(waiter) {
			return false
		}

		l.Unlock()

		return false
	}
}

func (l *RepoLock) removeQueuedWaiter(waiter *repoLockWaiter) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	for i, queued := range l.waiters {
		if queued == waiter {
			l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)

			return true
		}
	}

	return false
}

// Holder returns the jobID of the current lock holder.
func (l *RepoLock) Holder() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.holder
}

var repoLocks sync.Map // Map to hold locks for each repository

// stackLocks holds one stackLock per stack key, used to enforce mutual exclusion
// between a deployment and a scheduled job run for the same stack.
var stackLocks sync.Map

// GetRepoLock retrieves or creates a RepoLock for the given repoName.
func GetRepoLock(repoName string) *RepoLock {
	lockIface, _ := repoLocks.LoadOrStore(repoName, &RepoLock{})
	return lockIface.(*RepoLock)
}

// ErrStackBusy reports that a stack lock is held by another operation and the
// caller chose not to wait for it.
var ErrStackBusy = errors.New("stack is locked by another operation")

// unknownStackHolder describes a holder that did not give a description.
const unknownStackHolder = "another operation"

// StackBusyError tells which operation holds a busy stack lock.
type StackBusyError struct {
	Key    string
	Holder string
}

func (e *StackBusyError) Error() string {
	return fmt.Sprintf("stack %q is locked by %s", e.Key, e.Holder)
}

// Unwrap lets errors.Is match ErrStackBusy.
func (e *StackBusyError) Unwrap() error {
	return ErrStackBusy
}

// stackLock is a mutex that can be acquired with a context and that records
// a description of its current holder for log and skip messages.
type stackLock struct {
	sem    chan struct{}
	mu     sync.Mutex
	holder string
}

func newStackLock() *stackLock {
	return &stackLock{sem: make(chan struct{}, 1)}
}

func (l *stackLock) setHolder(holder string) {
	if holder == "" {
		holder = unknownStackHolder
	}

	l.mu.Lock()
	l.holder = holder
	l.mu.Unlock()
}

// currentHolder returns the holder description and whether the lock is held.
// The holder can be briefly empty right after another goroutine acquired the
// lock, so an empty description falls back to a generic text.
func (l *stackLock) currentHolder() (string, bool) {
	if len(l.sem) == 0 {
		return "", false
	}

	l.mu.Lock()
	holder := l.holder
	l.mu.Unlock()

	if holder == "" {
		holder = unknownStackHolder
	}

	return holder, true
}

func (l *stackLock) tryLock(holder string) bool {
	select {
	case l.sem <- struct{}{}:
		l.setHolder(holder)

		return true
	default:
		return false
	}
}

func (l *stackLock) lockContext(ctx context.Context, holder string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case l.sem <- struct{}{}:
		l.setHolder(holder)

		if err := ctx.Err(); err != nil {
			l.unlock()

			return err
		}

		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlock releases the lock. It panics when the lock is not held, mirroring
// sync.Mutex misuse semantics.
func (l *stackLock) unlock() {
	l.mu.Lock()
	l.holder = ""
	l.mu.Unlock()

	select {
	case <-l.sem:
	default:
		panic("lock: unlock of unlocked stack lock")
	}
}

// getStackLock returns the lock for the given stack key, creating it if needed.
func getStackLock(stackKey string) *stackLock {
	if existing, ok := stackLocks.Load(stackKey); ok {
		return existing.(*stackLock)
	}

	l, _ := stackLocks.LoadOrStore(stackKey, newStackLock())

	return l.(*stackLock)
}

// StackKey builds the per-stack lock key for a stack deployed to a Docker context.
// Stack names are only unique within a Docker context, so same-named stacks on
// different contexts must not block each other. The default context keeps the bare
// stack name so callers that only ever operate on it (the job scheduler and the
// certificate rotation watcher) stay mutually exclusive with its deployments.
func StackKey(contextName, stackName string) string {
	contextName = strings.TrimSpace(contextName)
	if contextName == "" || strings.EqualFold(contextName, "default") {
		return stackName
	}

	return contextName + "/" + stackName
}

// LockStack acquires the per-stack scheduler/deployment lock for stackName.
// While held, scheduled runs and deployments for this specific stack are mutually
// exclusive. Different stacks do not block each other.
// It blocks without a bound. Prefer LockStackContext, which can stop waiting.
// If stackName is empty the call is a no-op.
func LockStack(stackName string) {
	_ = LockStackContext(context.Background(), stackName, "")
}

// LockStackContext acquires the per-stack lock for stackKey and records holder
// as the description of the operation that holds it. It returns the context
// error when ctx ends before the lock is free.
// If stackKey is empty the call is a no-op.
func LockStackContext(ctx context.Context, stackKey, holder string) error {
	if stackKey == "" {
		return nil
	}

	return getStackLock(stackKey).lockContext(ctx, holder)
}

// TryLockStack acquires the per-stack lock for stackKey only when it is free.
// When the lock is busy, it returns false and the description of the holder.
// If stackKey is empty the call is a no-op and returns true.
func TryLockStack(stackKey, holder string) (bool, string) {
	if stackKey == "" {
		return true, ""
	}

	l := getStackLock(stackKey)
	if l.tryLock(holder) {
		return true, ""
	}

	current, _ := l.currentHolder()
	if current == "" {
		current = unknownStackHolder
	}

	return false, current
}

// StackLockHolder returns the description of the operation that holds the
// per-stack lock for stackKey, and whether the lock is held.
func StackLockHolder(stackKey string) (string, bool) {
	if stackKey == "" {
		return "", false
	}

	existing, ok := stackLocks.Load(stackKey)
	if !ok {
		return "", false
	}

	return existing.(*stackLock).currentHolder()
}

// AcquireStackOptions controls how AcquireStack acquires a stack lock.
type AcquireStackOptions struct {
	// Holder describes the operation that acquires the lock.
	Holder string
	// Wait makes AcquireStack wait for a busy lock until ctx ends.
	// Without Wait, a busy lock returns a *StackBusyError at once.
	Wait bool
	// OnWait is called once with the current holder before AcquireStack
	// starts to wait for a busy lock.
	OnWait func(holder string)
}

// AcquireStack acquires the per-stack lock for stackKey and returns a function
// that releases it. A busy lock either returns a *StackBusyError (which matches
// ErrStackBusy) or waits until ctx ends, based on opts.Wait.
// If stackKey is empty the call is a no-op.
func AcquireStack(ctx context.Context, stackKey string, opts AcquireStackOptions) (func(), error) {
	if stackKey == "" {
		return func() {}, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	release := func() { UnlockStack(stackKey) }

	ok, holder := TryLockStack(stackKey, opts.Holder)
	if ok {
		if err := ctx.Err(); err != nil {
			release()

			return nil, err
		}

		return release, nil
	}

	if !opts.Wait {
		return nil, &StackBusyError{Key: stackKey, Holder: holder}
	}

	if opts.OnWait != nil {
		opts.OnWait(holder)
	}

	if err := LockStackContext(ctx, stackKey, opts.Holder); err != nil {
		return nil, err
	}

	return release, nil
}

// UnlockStack releases the per-stack scheduler/deployment lock for stackName.
// If stackName is empty the call is a no-op.
func UnlockStack(stackName string) {
	if stackName == "" {
		return
	}

	getStackLock(stackName).unlock()
}
