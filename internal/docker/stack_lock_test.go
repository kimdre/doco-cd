package docker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/lock"
)

// stackLockLogBuffer is a log writer that is safe for concurrent use.
type stackLockLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *stackLockLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *stackLockLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func newStackLockTestLogger() (*slog.Logger, *stackLockLogBuffer) {
	buf := &stackLockLogBuffer{}

	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// stackLockTestName returns a stack name that is unique per test, because the stack lock map is global.
func stackLockTestName(t *testing.T) string {
	t.Helper()

	return strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
}

func TestAcquireStackLock(t *testing.T) {
	t.Parallel()

	const (
		contextName = "remote"
		holder      = `scheduled job "backup" (run run-1)`
	)

	t.Run("free lock", func(t *testing.T) {
		t.Parallel()

		stack := stackLockTestName(t)
		key := lock.StackKey(contextName, stack)

		locked, err := AcquireStackLock(t.Context(), nil, contextName, stack, StackLockOptions{Holder: "deployment"})
		if err != nil {
			t.Fatalf("acquireStackLock() error = %v", err)
		}

		if got, held := lock.StackLockHolder(key); !held || got != "deployment" {
			t.Fatalf("StackLockHolder() = %q, %v, want deployment, true", got, held)
		}

		locked.Release()

		if _, held := lock.StackLockHolder(key); held {
			t.Fatal("stack lock is still held after release")
		}
	})

	t.Run("skip if busy", func(t *testing.T) {
		t.Parallel()

		stack := stackLockTestName(t)
		key := lock.StackKey(contextName, stack)

		if err := lock.LockStackContext(t.Context(), key, holder); err != nil {
			t.Fatalf("LockStackContext() error = %v", err)
		}

		defer lock.UnlockStack(key)

		log, logs := newStackLockTestLogger()

		locked, err := AcquireStackLock(t.Context(), log, contextName, stack, StackLockOptions{Holder: "poll", SkipIfBusy: true})
		if err == nil {
			locked.Release()
			t.Fatal("acquireStackLock() error = nil, want busy error")
		}

		if !errors.Is(err, lock.ErrStackBusy) {
			t.Fatalf("acquireStackLock() error = %v, want ErrStackBusy", err)
		}

		var busy *lock.StackBusyError
		if !errors.As(err, &busy) {
			t.Fatalf("acquireStackLock() error = %T, want *lock.StackBusyError", err)
		}

		if busy.Key != key || busy.Holder != holder {
			t.Fatalf("StackBusyError = %+v, want key %q and holder %q", busy, key, holder)
		}

		if strings.Contains(logs.String(), "waiting for stack lock") {
			t.Fatalf("skip logged a wait message: %s", logs.String())
		}
	})

	t.Run("wait until context ends", func(t *testing.T) {
		t.Parallel()

		stack := stackLockTestName(t)
		key := lock.StackKey(contextName, stack)

		if err := lock.LockStackContext(t.Context(), key, holder); err != nil {
			t.Fatalf("LockStackContext() error = %v", err)
		}

		defer lock.UnlockStack(key)

		log, logs := newStackLockTestLogger()

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		locked, err := AcquireStackLock(ctx, log, contextName, stack, StackLockOptions{Holder: "webhook"})
		if err == nil {
			locked.Release()
			t.Fatal("acquireStackLock() error = nil, want context deadline")
		}

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("acquireStackLock() error = %v, want context.DeadlineExceeded", err)
		}

		output := logs.String()
		if !strings.Contains(output, "waiting for stack lock") || !strings.Contains(output, "lock_holder=") ||
			!strings.Contains(output, "run-1") {
			t.Fatalf("wait log = %q, want wait message with lock_holder", output)
		}

		if got, held := lock.StackLockHolder(key); !held || got != holder {
			t.Fatalf("StackLockHolder() = %q, %v, want original holder", got, held)
		}
	})

	t.Run("wait until holder releases", func(t *testing.T) {
		t.Parallel()

		stack := stackLockTestName(t)
		key := lock.StackKey(contextName, stack)

		if err := lock.LockStackContext(t.Context(), key, holder); err != nil {
			t.Fatalf("LockStackContext() error = %v", err)
		}

		log, logs := newStackLockTestLogger()

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		type result struct {
			stack *LockedStack
			err   error
		}

		done := make(chan result, 1)

		go func() {
			locked, err := AcquireStackLock(ctx, log, contextName, stack, StackLockOptions{Holder: "api"})
			done <- result{stack: locked, err: err}
		}()

		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(logs.String(), "waiting for stack lock") {
			if time.Now().After(deadline) {
				lock.UnlockStack(key)
				t.Fatal("acquireStackLock() did not log the wait message")
			}

			time.Sleep(5 * time.Millisecond)
		}

		lock.UnlockStack(key)

		got := <-done
		if got.err != nil {
			t.Fatalf("acquireStackLock() error = %v", got.err)
		}

		if current, held := lock.StackLockHolder(key); !held || current != "api" {
			t.Fatalf("StackLockHolder() = %q, %v, want api, true", current, held)
		}

		got.stack.Release()
	})
}
