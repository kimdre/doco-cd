package deploy

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/encryption"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// LoadExternalSecretsFiles processes local ExternalSecretsFiles entries and accumulates their
// contents into Config.Internal.ExternalSecretsFromFiles. Each file is a YAML document holding a
// map of env var name to external secret reference, using the same shape as ExternalSecrets.
// Files prefixed with "remote:" are left in Config.ExternalSecretsFiles (without the prefix) for
// later processing once a remote repository has been checked out, mirroring LoadLocalDotEnv.
// Call MergeExternalSecretsFromFiles once all local and remote files have been loaded.
func LoadExternalSecretsFiles(config *Config, basePath string) error {
	const remotePrefix = "remote:"

	var remoteFiles []string

	if config.Internal.ExternalSecretsFromFiles == nil {
		config.Internal.ExternalSecretsFromFiles = make(map[string]secrettypes.ExternalSecretRef)
	}

	for _, f := range config.ExternalSecretsFiles {
		if after, ok := strings.CutPrefix(f, remotePrefix); ok {
			remoteFiles = append(remoteFiles, after)
			continue
		}

		absPath := filepath.Join(basePath, f)

		isEncrypted, err := encryption.IsEncryptedFile(absPath)
		if err != nil {
			return fmt.Errorf("failed to check if external secrets file is encrypted %s: %w", absPath, err)
		}

		var content []byte

		if isEncrypted {
			content, err = encryption.DecryptFile(absPath)
			if err != nil {
				return fmt.Errorf("failed to decrypt external secrets file %s: %w", absPath, err)
			}
		} else {
			content, err = os.ReadFile(absPath)
			if err != nil {
				return fmt.Errorf("failed to read external secrets file %s: %w", absPath, err)
			}
		}

		refs := make(map[string]secrettypes.ExternalSecretRef)

		if err := yaml.Unmarshal(content, &refs); err != nil {
			return fmt.Errorf("failed to parse external secrets file %s: %w", absPath, err)
		}

		maps.Copy(config.Internal.ExternalSecretsFromFiles, refs)
	}

	config.ExternalSecretsFiles = remoteFiles

	return nil
}

// MergeExternalSecretsFromFiles merges secrets accumulated by LoadExternalSecretsFiles into
// Config.ExternalSecrets. Inline ExternalSecrets entries take precedence over file-sourced ones
// on key collision.
func MergeExternalSecretsFromFiles(config *Config) {
	if len(config.Internal.ExternalSecretsFromFiles) == 0 {
		return
	}

	merged := make(map[string]secrettypes.ExternalSecretRef, len(config.Internal.ExternalSecretsFromFiles)+len(config.ExternalSecrets))

	maps.Copy(merged, config.Internal.ExternalSecretsFromFiles)
	maps.Copy(merged, config.ExternalSecrets)

	config.ExternalSecrets = merged
}
