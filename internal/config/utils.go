package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"gopkg.in/validator.v2"

	"github.com/kimdre/doco-cd/internal/encryption"
)

// EnvVarFileMapping holds the mappings for file-based environment variables.
type EnvVarFileMapping struct {
	EnvName    string  // EnvName is the name/key of the environment variable (e.g. API_SECRET)
	EnvValue   *string // EnvValue is the value of the environment variable
	FileValue  *string // FileValue is the content of the file that is specified in the environment variable (e.g. API_SECRET_FILE)
	AllowUnset bool    // AllowUnset indicates if both the fileField and value can be unset
}

// LoadFileBasedEnvVars loads env-based config values from file-based env vars if set.
func LoadFileBasedEnvVars(mappings *[]EnvVarFileMapping) error {
	for _, m := range *mappings {
		fileSet := m.FileValue != nil && *m.FileValue != ""
		valSet := m.EnvValue != nil && *m.EnvValue != ""

		if fileSet {
			if valSet {
				return fmt.Errorf("%w: %s or %s", ErrBothSecretsSet, m.EnvName, m.EnvName+"_FILE")
			}

			*m.EnvValue = strings.TrimSpace(*m.FileValue)

			continue
		}

		if !valSet && !m.AllowUnset {
			return fmt.Errorf("%w: %s or %s", ErrBothSecretsNotSet, m.EnvName, m.EnvName+"_FILE")
		}
	}

	return nil
}

// ParseConfigFromEnv parses the configuration from environment variables and file-based environment variables.
func ParseConfigFromEnv(config interface{}, mappings *[]EnvVarFileMapping) error {
	// Parse the environment variables into the config struct
	// Also load any values from files if *_FILE env vars are set
	if err := env.Parse(config); err != nil {
		return err
	}

	if err := LoadFileBasedEnvVars(mappings); err != nil {
		return err
	}

	if err := validator.Validate(config); err != nil {
		return err
	}

	return nil
}

// LoadLocalDotEnv processes local dotenv files and loads their variables into the DeployConfig.Internal.Environment map.
func LoadLocalDotEnv(deployConfig *DeployConfig, internalRepoPath string) error {
	const remotePrefix = "remote:"

	var remoteEnvFiles []string // List of env files that are not local and will be processed later

	envVars := make(map[string]string)

	for _, f := range deployConfig.EnvFiles {
		// Process any env-files that are local and not in the remote repository (see repository_url)
		if !strings.HasPrefix(f, remotePrefix) {
			absPath := filepath.Join(internalRepoPath, f)

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

			for k, v := range envMap {
				envVars[k] = v
			}
		} else {
			f = strings.TrimPrefix(f, remotePrefix)
			remoteEnvFiles = append(remoteEnvFiles, f)
		}
	}

	deployConfig.EnvFiles = remoteEnvFiles
	deployConfig.Internal.Environment = envVars

	return nil
}

// CreateTmpDotEnvFile creates a temporary dotenv file from the DeployConfig.Internal.Environment map.
func CreateTmpDotEnvFile(deployConfig *DeployConfig) (string, error) {
	tmpEnvFile, err := os.CreateTemp(os.TempDir(), deployConfig.Name+".*.env")
	if err != nil {
		errMsg := "failed to create temporary env file"
		return "", fmt.Errorf("%s: %w", errMsg, err)
	}

	// Write environment variables to the temp env file
	for k, v := range deployConfig.Internal.Environment {
		_, err = fmt.Fprintf(tmpEnvFile, "%s=%s\n", k, v)
		if err != nil {
			return "", fmt.Errorf("failed to write to temporary env file: %w", err)
		}
	}

	if err = tmpEnvFile.Close(); err != nil {
		return "", fmt.Errorf("failed to close temporary env file: %w", err)
	}

	// Prepend the temp env file to the list of env files
	deployConfig.EnvFiles = append([]string{tmpEnvFile.Name()}, deployConfig.EnvFiles...)

	return tmpEnvFile.Name(), nil
}
