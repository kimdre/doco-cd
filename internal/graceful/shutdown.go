package graceful

import (
	"log/slog"
	"sync"
)

var (
	shutdownFuncs    []func()
	shutdownFuncLock sync.Mutex
)

// RegistryShutdownFunc registers function called on shutdown.
func RegistryShutdownFunc(f func()) {
	shutdownFuncLock.Lock()
	defer shutdownFuncLock.Unlock()

	shutdownFuncs = append(shutdownFuncs, f)
}

func getRegisteredShutdownFuncs() []func() {
	shutdownFuncLock.Lock()
	defer shutdownFuncLock.Unlock()

	return shutdownFuncs
}

func runRegisteredShutdownFuncs(log *slog.Logger) {
	funcs := getRegisteredShutdownFuncs()

	log.Info("calling registered shutdown functions")

	for _, f := range funcs {
		f()
	}

	log.Info("finished registered shutdown functions")
}
