package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"gopkg.in/validator.v2"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
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
func LoadLocalDotEnv(deployConfig *DeployConfig, internalRepoPath, envFilesDir string) error {
	const (
		remotePrefix = "remote:"
		filePrefix   = "file:"
	)

	var remoteEnvFiles []string // List of env files that are not local and will be processed later

	envVars := make(map[string]string)

	for _, f := range deployConfig.EnvFiles {
		switch {
		case strings.HasPrefix(f, remotePrefix):
			remotePath := strings.TrimSpace(strings.TrimPrefix(f, remotePrefix))
			if remotePath == "" {
				return fmt.Errorf("%w: remote env file entry is empty", ErrInvalidFilePath)
			}

			remoteEnvFiles = append(remoteEnvFiles, remotePath)
		case strings.HasPrefix(f, filePrefix):
			if envFilesDir == "" {
				return fmt.Errorf("%w: file env file entries require ENV_DIR to be configured", ErrInvalidFilePath)
			}

			relPath := strings.TrimSpace(strings.TrimPrefix(f, filePrefix))
			if relPath == "" {
				return fmt.Errorf("%w: file env file entry is empty", ErrInvalidFilePath)
			}

			if filepath.IsAbs(relPath) {
				return fmt.Errorf("%w: file env file paths must be relative: %s", ErrInvalidFilePath, relPath)
			}

			relPath = filepath.Clean(relPath)
			trustedRoot := filepath.Clean(envFilesDir)

			absPath, err := filesystem.VerifyAndSanitizePath(filepath.Join(trustedRoot, relPath), trustedRoot)
			if err != nil {
				if errors.Is(err, filesystem.ErrPathTraversal) {
					return fmt.Errorf("%w: %s", ErrInvalidFilePath, relPath)
				}

				return fmt.Errorf("failed to verify env file path %s: %w", relPath, err)
			}

			if err := mergeEnvFile(absPath, envVars, false); err != nil {
				return err
			}
		default:
			absPath := filepath.Join(internalRepoPath, f)
			allowMissing := f == ".env"

			if err := mergeEnvFile(absPath, envVars, allowMissing); err != nil {
				return err
			}
		}
	}

	deployConfig.EnvFiles = remoteEnvFiles
	deployConfig.Internal.Environment = envVars

	return nil
}

func mergeEnvFile(path string, envVars map[string]string, allowMissing bool) error {
	envMap, err := readEnvFile(path, allowMissing)
	if err != nil {
		return err
	}

	for k, v := range envMap {
		envVars[k] = v
	}

	return nil
}

func readEnvFile(path string, allowMissing bool) (map[string]string, error) {
	isEncrypted, err := encryption.IsEncryptedFile(path)
	if err != nil {
		if allowMissing && os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to check if env file is encrypted %s: %w", path, err)
	}

	if isEncrypted {
		decryptedContent, err := encryption.DecryptFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt env file %s: %w", path, err)
		}

		envMap, err := godotenv.UnmarshalBytes(decryptedContent)
		if err != nil {
			return nil, fmt.Errorf("failed to parse decrypted env file %s: %w", path, err)
		}

		return envMap, nil
	}

	envMap, err := godotenv.Read(path)
	if err != nil {
		if allowMissing && os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to read env file %s: %w", path, err)
	}

	return envMap, nil
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
