package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

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
	fields := []struct {
		fileField string
		value     *string
		name      string
	}{
		{cfg.WebhookSecretFile, &cfg.WebhookSecret, "WEBHOOK_SECRET"},
		{cfg.GitAccessTokenFile, &cfg.GitAccessToken, "GIT_ACCESS_TOKEN"},
	}

	for _, field := range fields {
		if field.fileField != "" {
			if *field.value != "" {
				return fmt.Errorf("%w: %s or %s", ErrBothSecretsSet, field.name, field.name+"_FILE")
			}

			*field.value = field.fileField
		} else if *field.value == "" {
			return fmt.Errorf("%w: %s or %s", ErrBothSecretsNotSet, field.name, field.name+"_FILE")
		}
	}

	return nil
}

// validateUniqueProjectNames checks if the project names in the configs are unique.
func validateUniqueProjectNames(configs []*DeployConfig) error {
	names := make(map[string]bool)
	for _, config := range configs {
		if names[config.Name] {
			return fmt.Errorf("%w: %s", ErrDuplicateProjectName, config.Name)
		}

		names[config.Name] = true
	}

	return nil
}
