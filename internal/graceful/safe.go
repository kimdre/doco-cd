package graceful

import (
	"log/slog"
	"sync"
)

// SafeGo starts a new goroutine and adds it to wg.
// it also recovers from any panic in the goroutine and logs it using the provided logger.
func SafeGo(wg *sync.WaitGroup, log *slog.Logger, f func()) {
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
