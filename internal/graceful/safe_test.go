package graceful_test

import (
	"log/slog"
	"sync"
	"testing"

	"github.com/kimdre/doco-cd/internal/graceful"
)

func TestSafeGo(t *testing.T) {
	t.Parallel()

	wg := &sync.WaitGroup{}

	defer wg.Wait()

	log := slog.Default()
	graceful.SafeGo(wg, log, func() {
		t.Logf("test output with panic")
		panic("test panic")
	})
	graceful.SafeGo(wg, log, func() {
		t.Logf("test output with no panic")
	})
}
