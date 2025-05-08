package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
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

// loadEnvFromDockerSecrets loads app configuration values from Docker Secrets to environment variables.
func loadEnvFromDockerSecrets(secretsPath string) error {
	files, err := os.ReadDir(secretsPath)
	if err != nil {
		if os.IsNotExist(err) {
			// If the directory does not exist, docker secrets are not used.
			return nil
		}

		return fmt.Errorf("failed to read Docker secrets: %v", err)
	}

	for _, file := range files {
		secretName := file.Name()

		secretValue, err := os.ReadFile(fmt.Sprintf("%s/%s", secretsPath, secretName))
		if err != nil {
			return fmt.Errorf("failed to read secret %s: %v", secretName, err)
		}

		err = os.Setenv(strings.ToUpper(secretName), strings.TrimSpace(string(secretValue)))
		if err != nil {
			return fmt.Errorf("failed to set environment variable %s: %v", secretName, err)
		}
	}

	return nil
}
