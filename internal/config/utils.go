package config

import (
	"fmt"
	"strings"

	"github.com/caarlos0/env/v11"
	"gopkg.in/validator.v2"
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
