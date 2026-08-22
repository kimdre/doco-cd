package deploy

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"

	"github.com/kimdre/doco-cd/internal/encryption"
)

// LoadLocalDotEnv processes local dotenv files and loads their variables into the Config.Internal Environment map.
// Remote dotenv files (prefixed with "remote:") are collected and left in Config.EnvFiles for later processing.
//
// Env files are parsed with compose-go's lookup-aware parser so that variable references (e.g. VAR=${VAR})
// resolve against values already accumulated from previously processed files, matching the cascading
// resolution behavior compose-go itself uses when loading multiple --env-file entries. Without this,
// a later, more specific env file using a self-referencing placeholder (common in gitops setups that
// declare an env var per-directory to be filled in from a broader-scope .env file) would resolve to an
// empty string and silently overwrite a correctly resolved value from an earlier file.
func LoadLocalDotEnv(config *Config, basePath string) error {
	const remotePrefix = "remote:"

	var remoteEnvFiles []string // List of env files that are not local and will be processed later

	if len(config.Internal.Environment) == 0 {
		config.Internal.Environment = make(map[string]string)
	}

	lookupFn := func(key string) (string, bool) {
		v, ok := config.Internal.Environment[key]
		return v, ok
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

			var content []byte

			if isEncrypted {
				content, err = encryption.DecryptFile(absPath)
				if err != nil {
					return fmt.Errorf("failed to decrypt env file %s: %w", absPath, err)
				}
			} else {
				content, err = os.ReadFile(absPath)
				if err != nil {
					return fmt.Errorf("failed to read local env file %s: %w", absPath, err)
				}
			}

			envMap, err := dotenv.UnmarshalBytesWithLookup(content, lookupFn)
			if err != nil {
				return fmt.Errorf("failed to parse env file %s: %w", absPath, err)
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
