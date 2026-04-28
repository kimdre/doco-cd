package graceful

import (
	"log/slog"
	"sync"
)

// RegistryShutdownFunc registers function called on shutdown.
func (h *handler) RegistryShutdownFunc(name string, f func()) {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.shutdownFuncs = append(h.shutdownFuncs, shutdownFunc{
		name: name,
		f:    f,
	})
}

// RegistryShutdownFunc registers function called on shutdown to default handler.
func RegistryShutdownFunc(name string, f func()) {
	defaultHandler.RegistryShutdownFunc(name, f)
}

func (h *handler) getRegisteredShutdownFuncs() []shutdownFunc {
	h.lock.Lock()
	defer h.lock.Unlock()

	return h.shutdownFuncs
}

func (h *handler) runRegisteredShutdownFuncs(log *slog.Logger) {
	funcs := h.getRegisteredShutdownFuncs()

	log.Info("calling registered shutdown functions")

	wg := sync.WaitGroup{}
	for _, f := range funcs {
		SafeGo(&wg, log, func() {
			log.Info("started run shutdown function", slog.String("name", f.name))
			defer log.Info("finished run shutdown function", slog.String("name", f.name))

			f.f()
		})
	}

	wg.Wait()

	log.Info("finished registered shutdown functions")
}
