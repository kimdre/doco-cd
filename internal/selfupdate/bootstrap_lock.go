package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	bootstrapLockFileName = ".self-update-bootstrap.lock"
	lockRetryInterval     = 25 * time.Millisecond
)

// WithBootstrapLock runs bootstrap deployments while excluding managed poll runs.
func WithBootstrapLock(ctx context.Context, dataMountPath string, run func() error) error {
	return withDataMountLock(ctx, dataMountPath, unix.LOCK_EX, run)
}

// WithPollLock lets poll runs overlap each other while waiting for bootstrap to finish.
func WithPollLock(ctx context.Context, dataMountPath string, run func() error) error {
	return withDataMountLock(ctx, dataMountPath, unix.LOCK_SH, run)
}

func withDataMountLock(ctx context.Context, dataMountPath string, mode int, run func() error) (runErr error) {
	if run == nil {
		return errors.New("self-update lock requires a run function")
	}

	release, err := acquireDataMountLock(ctx, dataMountPath, mode)
	if err != nil {
		return err
	}

	defer func() {
		if releaseErr := release(); releaseErr != nil {
			runErr = errors.Join(runErr, releaseErr)
		}
	}()

	return run()
}

func acquireDataMountLock(ctx context.Context, dataMountPath string, mode int) (func() error, error) {
	if dataMountPath == "" {
		return nil, errors.New("self-update lock requires a data mount path")
	}

	if err := os.MkdirAll(dataMountPath, 0o750); err != nil {
		return nil, fmt.Errorf("create data mount path for self-update lock: %w", err)
	}

	lockPath := filepath.Join(dataMountPath, bootstrapLockFileName)

	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open self-update lock file: %w", err)
	}

	closeOnError := func(cause error) error {
		if closeErr := file.Close(); closeErr != nil {
			return errors.Join(cause, fmt.Errorf("close self-update lock file: %w", closeErr))
		}

		return cause
	}

	ticker := time.NewTicker(lockRetryInterval)
	defer ticker.Stop()

	for {
		if err = ctx.Err(); err != nil {
			return nil, closeOnError(fmt.Errorf("wait for self-update lock: %w", err))
		}

		err = unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			release := releaseDataMountLock(file)
			if err = ctx.Err(); err != nil {
				return nil, errors.Join(fmt.Errorf("wait for self-update lock: %w", err), release())
			}

			return release, nil
		}

		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return nil, closeOnError(fmt.Errorf("acquire self-update lock: %w", err))
		}

		select {
		case <-ctx.Done():
			return nil, closeOnError(fmt.Errorf("wait for self-update lock: %w", ctx.Err()))
		case <-ticker.C:
		}
	}
}

func releaseDataMountLock(file *os.File) func() error {
	var (
		once       sync.Once
		releaseErr error
	)

	return func() error {
		once.Do(func() {
			if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
				releaseErr = fmt.Errorf("unlock self-update lock file: %w", err)
			}

			if err := file.Close(); err != nil {
				releaseErr = errors.Join(releaseErr, fmt.Errorf("close self-update lock file: %w", err))
			}
		})

		return releaseErr
	}
}
