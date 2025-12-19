package encryption

import (
	"os"
	"strings"
)

// SopsKeyIsSet checks if an env var starting with SOPS_ is set.
func SopsKeyIsSet() bool {
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "SOPS_") {
			// Check if the SOPS env var has a value
			parts := strings.SplitN(env, "=", 2)
			if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
				// SOPS env var is set and has a value
				return true
			}
		}
	}

	return false
}
