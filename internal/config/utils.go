package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/creasty/defaults"

	"gopkg.in/yaml.v3"
)

func (c *DeployConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	err := defaults.Set(c)
	if err != nil {
		return err
	}

	type Plain DeployConfig

	if err := unmarshal((*Plain)(c)); err != nil {
		return err
	}

	return nil
}

func FromYAML(f string) ([]*DeployConfig, error) {
	b, err := os.ReadFile(f)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %v", err)
	}

	// Read all yaml documents in the file and unmarshal them into a slice of DeployConfig structs
	dec := yaml.NewDecoder(bytes.NewReader(b))

	var configs []*DeployConfig

	for {
		var c DeployConfig

		err = dec.Decode(&c)
		if err != nil {
			if err == io.EOF {
				break
			}

			return nil, fmt.Errorf("failed to decode yaml: %v", err)
		}

		configs = append(configs, &c)
	}

	if len(configs) == 0 {
		return nil, errors.New("no yaml documents found in file")
	}

	return configs, nil
}

// loadFileBasedEnvVars loads environment variables from files if the corresponding file-based environment variable is set.
func loadFileBasedEnvVars(cfg *AppConfig) error {
	if cfg.WebhookSecretFile != "" {
		if cfg.WebhookSecret != "" && cfg.WebhookSecretFile != "" {
			return fmt.Errorf("%w: %s or %s", ErrBothSecretsSet, "WEBHOOK_SECRET", "WEBHOOK_SECRET_FILE")
		}

		cfg.WebhookSecret = cfg.WebhookSecretFile
	} else if cfg.WebhookSecret == "" {
		return fmt.Errorf("%w: %s or %s", ErrBothSecretsNotSet, "WEBHOOK_SECRET", "WEBHOOK_SECRET_FILE")
	}

	if cfg.GitAccessTokenFile != "" {
		if cfg.GitAccessToken != "" && cfg.GitAccessTokenFile != "" {
			return fmt.Errorf("%w: %s or %s", ErrBothSecretsSet, "GIT_ACCESS_TOKEN", "GIT_ACCESS_TOKEN_FILE")
		}

		cfg.GitAccessToken = cfg.GitAccessTokenFile
	} else if cfg.GitAccessToken == "" {
		return fmt.Errorf("%w: %s or %s", ErrBothSecretsSet, "GIT_ACCESS_TOKEN", "GIT_ACCESS_TOKEN_FILE")
	}

	return nil
}

// CamelCaseToSnakeCase converts a string from camelCase to snake_case.
func CamelCaseToSnakeCase(str string) string {
	matchFirstCap := regexp.MustCompile("(.)([A-Z][a-z]+)")
	matchAllCap := regexp.MustCompile("([a-z0-9])([A-Z])")

	snake := matchFirstCap.ReplaceAllString(str, "${1}_${2}")
	snake = matchAllCap.ReplaceAllString(snake, "${1}_${2}")

	return strings.ToLower(snake)
}
