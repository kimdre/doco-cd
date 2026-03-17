package encryption

import (
	"os"
	"strings"
	"sync"
)

var (
	checkSopsKeyOnce sync.Once
	sopsKeySet       bool
)

// SopsKeyIsSet checks if an env var starting with SOPS_ is set.
// It runs only once and caches the result.
func SopsKeyIsSet() bool {
	checkSopsKeyOnce.Do(func() {
		for _, env := range os.Environ() {
			if strings.HasPrefix(env, "SOPS_") {
				parts := strings.SplitN(env, "=", 2)
				if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
					sopsKeySet = true
					return
				}
			}
		}

		sopsKeySet = false
	})

	return sopsKeySet
}
