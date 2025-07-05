package encryption

import (
	"os"
	"strings"
)

// SopsKeyIsSet checks if an env var starting with SOPS_ is set.
func SopsKeyIsSet() bool {
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "SOPS_") {
			return true
		}
	}

	return false
}
