package graceful

import (
	"log/slog"
	"sync/atomic"
	"testing"
)

func TestRunRegisteredShutdownFuncs(t *testing.T) {
	t.Parallel()

	called := atomic.Bool{}

	RegistryShutdownFunc(func() {
		called.Store(true)
	})
	runRegisteredShutdownFuncs(slog.Default())

	if !called.Load() {
		t.Errorf("expected called to be true")
	}
}
