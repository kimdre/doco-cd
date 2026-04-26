package graceful

import "sync"

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

func getShutdownFuncs() []func() {
	shutdownFuncLock.Lock()
	defer shutdownFuncLock.Unlock()

	return shutdownFuncs
}
