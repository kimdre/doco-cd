package controlplane

import (
	"errors"
	"testing"
	"time"
)

func TestBackgroundWorkCloseWaitsForRegistration(t *testing.T) {
	t.Parallel()

	work := newBackgroundWork()

	release, err := work.Register()
	if err != nil {
		t.Fatal(err)
	}

	closed := make(chan struct{})

	go func() {
		work.CloseAndWait()
		close(closed)
	}()

	select {
	case <-closed:
		t.Fatal("CloseAndWait returned before registration was released")
	case <-time.After(20 * time.Millisecond):
	}

	release()

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("CloseAndWait did not return after registration was released")
	}
}

func TestBackgroundWorkGoRunsAndCompletesBeforeClose(t *testing.T) {
	t.Parallel()

	work := newBackgroundWork()
	ran := make(chan struct{})

	if err := work.Go(func() { close(ran) }); err != nil {
		t.Fatalf("Go returned error before shutdown: %v", err)
	}

	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("background function did not run")
	}

	done := make(chan struct{})

	go func() {
		work.CloseAndWait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("CloseAndWait did not return after background function completed")
	}
}

func TestBackgroundWorkRejectsRegistrationDuringShutdown(t *testing.T) {
	t.Parallel()

	work := newBackgroundWork()

	release, err := work.Register()
	if err != nil {
		t.Fatal(err)
	}

	closed := make(chan struct{})

	go func() {
		work.CloseAndWait()
		close(closed)
	}()

	deadline := time.Now().Add(time.Second)

	for {
		var extraRelease func()

		extraRelease, err = work.Register()
		if errors.Is(err, ErrBackgroundWorkClosed) {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		extraRelease()

		if time.Now().After(deadline) {
			t.Fatal("registration was not rejected during shutdown")
		}

		time.Sleep(time.Millisecond)
	}

	release()
	<-closed

	if _, err := work.Register(); !errors.Is(err, ErrBackgroundWorkClosed) {
		t.Fatalf("registration after shutdown error = %v", err)
	}
}
