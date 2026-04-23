package graceful

import (
	"log/slog"
	"sync"
)

// Go starts a new goroutine and adds it to the global wait group.
// it also recovers from any panic in the goroutine and logs it using the provided logger.
func Go(wg *sync.WaitGroup, log *slog.Logger, f func()) {
	wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				// log the panic if needed
				log.Error("goroutine panicked", slog.Any("recover", r))
			}
		}()

		f()
	})
}

// Wait for all goroutines to finish.
func Wait(wg *sync.WaitGroup) {
	wg.Wait()
}
