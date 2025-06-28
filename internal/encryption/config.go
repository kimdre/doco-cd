package encryption

//type SopsConfig struct {
//	SopsAgeKey     string `env:"SOPS_AGE_KEY"`      // SopsAgeKey is the path to the SOPS age key file used for decryption of secrets (loaded by the SOPS library)
//	SopsAgeKeyFile string `env:"SOPS_AGE_KEY_FILE"` // SopsAgeKeyFile is the file containing the SOPS age key (loaded by the SOPS library)
//}

// SopsKeyIsSet checks if an env var starting with SOPS_ is set
//func SopsKeyIsSet() bool {
//	for _, env := range os.Environ() {
//		if len(env) >= 5 && env[:5] == "SOPS_" {
//			return true
//		}
//	}
//	return false
//}
