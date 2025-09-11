package config

import (
	"fmt"
	"strings"
)

// EnvVarFileMapping holds the mappings for file-based environment variables.
type EnvVarFileMapping struct {
	EnvName    string  // EnvName is the name/key of the environment variable (e.g. API_SECRET)
	EnvValue   *string // EnvValue is a pointer to the value of the environment variable
	FileValue  string  // FileValue is the value of the file-based environment variable (e.g. API_SECRET_FILE)
	AllowUnset bool    // AllowUnset indicates if both the fileField and value can be unset
}

// LoadFileBasedEnvVars loads environment variables from files if the corresponding file-based environment variable is set.
func LoadFileBasedEnvVars(mappings *[]EnvVarFileMapping) error {
	for _, m := range *mappings {
		if m.FileValue != "" {
			if *m.EnvValue != "" {
				return fmt.Errorf("%w: %s or %s", ErrBothSecretsSet, m.EnvName, m.EnvName+"_FILE")
			}

			*m.EnvValue = strings.TrimSpace(m.FileValue)
		} else if *m.EnvValue == "" && !m.AllowUnset {
			return fmt.Errorf("%w: %s or %s", ErrBothSecretsNotSet, m.EnvName, m.EnvName+"_FILE")
		}
	}

	return nil
}
