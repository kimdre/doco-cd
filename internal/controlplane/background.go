package controlplane

import (
	"errors"
	"sync"
)

// ErrBackgroundWorkClosed indicates that shutdown has stopped accepting work.
var ErrBackgroundWorkClosed = errors.New("application is shutting down")

// backgroundWork closes admission atomically before waiting for registered work.
type backgroundWork struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newBackgroundWork() *backgroundWork {
	return &backgroundWork{}
}

// Register admits work and returns an idempotent completion callback.
func (w *backgroundWork) Register() (func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil, ErrBackgroundWorkClosed
	}

	w.wg.Add(1)

	return sync.OnceFunc(w.wg.Done), nil
}

// Go starts admitted work in a goroutine and releases it on completion.
func (w *backgroundWork) Go(run func()) error {
	release, err := w.Register()
	if err != nil {
		return err
	}

	go func() {
		defer release()

		run()
	}()

	return nil
}

// Close prevents future work from being registered.
func (w *backgroundWork) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

// Wait blocks until all registered work has completed.
func (w *backgroundWork) Wait() {
	w.wg.Wait()
}

// CloseAndWait closes admission before waiting for active work.
func (w *backgroundWork) CloseAndWait() {
	w.Close()
	w.Wait()
}
