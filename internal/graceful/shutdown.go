package graceful

import (
	"log/slog"
)

// RegistryShutdownFunc registers function called on shutdown.
func (h *handler) RegistryShutdownFunc(f func()) {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.shutdownFuncs = append(h.shutdownFuncs, f)
}

// RegistryShutdownFunc registers function called on shutdown to default handler.
func RegistryShutdownFunc(f func()) {
	defaultHandler.RegistryShutdownFunc(f)
}

func (h *handler) getRegisteredShutdownFuncs() []func() {
	h.lock.Lock()
	defer h.lock.Unlock()

	return h.shutdownFuncs
}

func (h *handler) runRegisteredShutdownFuncs(log *slog.Logger) {
	funcs := h.getRegisteredShutdownFuncs()

	log.Info("calling registered shutdown functions")

	for _, f := range funcs {
		f()
	}

	log.Info("finished registered shutdown functions")
}
