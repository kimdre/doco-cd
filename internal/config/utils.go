package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
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
	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if strings.HasSuffix(field.Name, "File") {
			fileField := field
			// Get the corresponding non-File field
			normalFieldName := strings.TrimSuffix(fileField.Name, "File")
			normalField := v.FieldByName(normalFieldName)

			if !normalField.IsValid() {
				continue
			}

			if normalField.String() != "" && v.Field(i).String() != "" {
				return errors.New("both " + normalFieldName + " and " + fileField.Name + " are set, please set only one")
			}

			normalField.SetString(strings.TrimSpace(v.Field(i).String()))
		}
	}

	return nil
}
