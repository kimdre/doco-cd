package main

import (
	"errors"
	"sync"
)

var errBackgroundWorkClosed = errors.New("application is shutting down")

type backgroundWork struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newBackgroundWork() *backgroundWork {
	return &backgroundWork{}
}

func (w *backgroundWork) Go(run func()) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return errBackgroundWorkClosed
	}

	w.wg.Go(run)

	return nil
}

func (w *backgroundWork) CloseAndWait() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()

	w.wg.Wait()
}
