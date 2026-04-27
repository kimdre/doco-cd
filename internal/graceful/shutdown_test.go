package graceful

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestRunRegisteredShutdownFuncs(t *testing.T) {
	t.Parallel()

	called := atomic.Bool{}
	handler := newHandler()
	handler.RegistryShutdownFunc(func() {
		called.Store(true)
	})
	handler.RegisterServerFunc("svc", func(_ context.Context) error {
		return nil
	}, func(_ context.Context) error {
		return nil
	})

	if err := handler.Serve(getLog()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !called.Load() {
		t.Fatalf("expected called to be true")
	}
}
