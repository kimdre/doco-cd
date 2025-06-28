package encryption

import "os"

var SopsKeyIsSet = checkSopsKeyIsSet() // SopsKeyIsSet indicates if any SOPS environment variable is set, which is used to determine if SOPS should be used for decryption.

// checkSopsKeyIsSet checks if an env var starting with SOPS_ is set
func checkSopsKeyIsSet() bool {
	for _, env := range os.Environ() {
		if len(env) >= 5 && env[:5] == "SOPS_" {
			return true
		}
	}

	return false
}
