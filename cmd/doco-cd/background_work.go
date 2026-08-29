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

func (w *backgroundWork) Register() (func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil, errBackgroundWorkClosed
	}

	w.wg.Add(1)

	return sync.OnceFunc(w.wg.Done), nil
}

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

func (w *backgroundWork) CloseAndWait() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()

	w.wg.Wait()
}
