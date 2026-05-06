package deploy

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"

	"github.com/kimdre/doco-cd/internal/encryption"
)

// LoadLocalDotEnv processes local dotenv files and loads their variables into the Config.Internal Environment map.
// Remote dotenv files (prefixed with "remote:") are collected and left in Config.EnvFiles for later processing.
func LoadLocalDotEnv(config *Config, basePath string) error {
	const remotePrefix = "remote:"

	var remoteEnvFiles []string // List of env files that are not local and will be processed later

	if len(config.Internal.Environment) == 0 {
		config.Internal.Environment = make(map[string]string)
	}

	for _, f := range config.EnvFiles {
		// Process any env-files that are local and not in the remote repository (see repository_url)
		if !strings.HasPrefix(f, remotePrefix) {
			absPath := filepath.Join(basePath, f)

			// Decrypt file if needed
			isEncrypted, err := encryption.IsEncryptedFile(absPath)
			if err != nil {
				if os.IsNotExist(err) && f == ".env" {
					// It's okay if the default .env file doesn't exist
					continue
				}

				return fmt.Errorf("failed to check if env file is encrypted %s: %w", absPath, err)
			}

			var envMap map[string]string

			if isEncrypted {
				decryptedContent, err := encryption.DecryptFile(absPath)
				if err != nil {
					return fmt.Errorf("failed to decrypt env file %s: %w", absPath, err)
				}

				envMap, err = godotenv.UnmarshalBytes(decryptedContent)
				if err != nil {
					return fmt.Errorf("failed to parse decrypted env file %s: %w", absPath, err)
				}
			} else {
				envMap, err = godotenv.Read(absPath)
				if err != nil {
					return fmt.Errorf("failed to read local env file %s: %w", absPath, err)
				}
			}

			maps.Copy(config.Internal.Environment, envMap)
		} else {
			f = strings.TrimPrefix(f, remotePrefix)
			remoteEnvFiles = append(remoteEnvFiles, f)
		}
	}

	config.EnvFiles = remoteEnvFiles

	return nil
}
