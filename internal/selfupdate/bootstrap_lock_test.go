package selfupdate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func holdBootstrapLock(t *testing.T, dataMountPath string) func() {
	t.Helper()

	bootstrapStarted := make(chan struct{})
	bootstrapDone := make(chan struct{})
	releaseBootstrap := make(chan struct{})

	var releaseOnce sync.Once

	release := func() {
		releaseOnce.Do(func() {
			close(releaseBootstrap)
		})
	}

	var bootstrapErr error

	go func() {
		bootstrapErr = WithBootstrapLock(context.Background(), dataMountPath, func() error {
			close(bootstrapStarted)
			<-releaseBootstrap

			return nil
		})

		close(bootstrapDone)
	}()

	t.Cleanup(func() {
		release()

		select {
		case <-bootstrapDone:
			if bootstrapErr != nil {
				t.Errorf("bootstrap lock holder returned an error: %v", bootstrapErr)
			}
		case <-time.After(time.Second):
			t.Error("bootstrap lock holder did not finish")
		}
	})

	select {
	case <-bootstrapStarted:
	case <-time.After(time.Second):
		t.Fatal("bootstrap did not acquire its lock")
	}

	return release
}

func TestBootstrapLockBlocksPollUntilReleased(t *testing.T) {
	t.Parallel()

	dataMountPath := t.TempDir()
	releaseBootstrap := holdBootstrapLock(t, dataMountPath)

	pollStarted := make(chan struct{})
	pollDone := make(chan struct{})

	var pollErr error
	go func() {
		pollErr = WithPollLock(context.Background(), dataMountPath, func() error {
			close(pollStarted)

			return nil
		})

		close(pollDone)
	}()

	select {
	case <-pollStarted:
		t.Fatal("poll started while bootstrap held the exclusive lock")
	case <-time.After(50 * time.Millisecond):
	}

	releaseBootstrap()

	select {
	case <-pollStarted:
	case <-time.After(time.Second):
		t.Fatal("poll did not start after bootstrap released its lock")
	}

	select {
	case <-pollDone:
	case <-time.After(time.Second):
		t.Fatal("poll did not finish")
	}

	if pollErr != nil {
		t.Errorf("poll returned an error: %v", pollErr)
	}
}

func TestPollLocksCanOverlap(t *testing.T) {
	t.Parallel()

	dataMountPath := t.TempDir()
	pollStarted := make(chan struct{}, 2)
	pollDone := make(chan error, 2)
	releasePolls := make(chan struct{})

	var releaseOnce sync.Once

	release := func() {
		releaseOnce.Do(func() {
			close(releasePolls)
		})
	}
	defer release()

	run := func() error {
		pollStarted <- struct{}{}

		<-releasePolls

		return nil
	}

	for range 2 {
		go func() {
			pollDone <- WithPollLock(context.Background(), dataMountPath, run)
		}()
	}

	for range 2 {
		select {
		case <-pollStarted:
		case <-time.After(time.Second):
			t.Fatal("poll locks serialized independent poll runs")
		}
	}

	release()

	for range 2 {
		select {
		case err := <-pollDone:
			if err != nil {
				t.Errorf("poll returned an error: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("poll did not finish after the locks were released")
		}
	}
}

func TestPollLockHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	dataMountPath := t.TempDir()
	holdBootstrapLock(t, dataMountPath)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	pollRan := make(chan struct{})

	pollDone := make(chan error, 1)
	go func() {
		pollDone <- WithPollLock(ctx, dataMountPath, func() error {
			close(pollRan)

			return nil
		})
	}()

	select {
	case err := <-pollDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("poll error = %v, want context deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("poll did not stop waiting after context cancellation")
	}

	select {
	case <-pollRan:
		t.Fatal("poll ran after its context was canceled")
	default:
	}
}

func TestBootstrapLockReleasesWhenRunFails(t *testing.T) {
	t.Parallel()

	dataMountPath := t.TempDir()
	wantErr := errors.New("bootstrap failed")

	err := WithBootstrapLock(context.Background(), dataMountPath, func() error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("bootstrap error = %v, want %v", err, wantErr)
	}

	if err = WithPollLock(context.Background(), dataMountPath, func() error { return nil }); err != nil {
		t.Fatalf("poll after failed bootstrap: %v", err)
	}
}
