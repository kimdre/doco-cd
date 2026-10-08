// Package main generates the deployment configuration schema used by editors.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/kimdre/doco-cd/internal/common/defaults"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const (
	schemaPath = "wiki/docs/schemas/deploy.schema.json"
	schemaURL  = "https://doco.cd/latest/schemas/deploy.schema.json"
)

func main() {
	output := flag.String("output", schemaPath, "output file, or - for standard output")

	flag.Parse()

	if err := run(*output); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)

		os.Exit(1)
	}
}

func run(output string) error {
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	data, err := generate(root)
	if err != nil {
		return err
	}

	if output == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			return fmt.Errorf("write schema: %w", err)
		}

		return nil
	}

	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("create schema directory: %w", err)
	}

	//nolint:gosec // The schema is a public documentation asset.
	if err := os.WriteFile(output, data, 0o644); err != nil {
		return fmt.Errorf("write schema: %w", err)
	}

	return nil
}

func generate(root string) ([]byte, error) {
	descriptions, err := readDescriptions(root)
	if err != nil {
		return nil, err
	}

	version, err := docoCDVersion(root)
	if err != nil {
		return nil, err
	}

	schema, err := jsonschema.For[deploy.Config](nil)
	if err != nil {
		return nil, fmt.Errorf("infer deployment schema: %w", err)
	}

	var defaultConfig deploy.Config
	if err := defaults.Set(&defaultConfig); err != nil {
		return nil, fmt.Errorf("read deployment defaults: %w", err)
	}

	if err := describe(schema, reflect.TypeFor[deploy.Config](), reflect.ValueOf(defaultConfig), descriptions); err != nil {
		return nil, err
	}

	schema.ID = schemaURL
	schema.Title = "Doco-CD deployment configuration"
	schema.Schema = "http://json-schema.org/draft-07/schema#"
	schema.Description = "One deployment configuration per YAML document. Fields may be omitted in auto-discovery overrides; required fields and deployment-dependent rules are checked by doco-cd at runtime."
	schema.Comment = "Generated automatically from Doco-CD's Go configuration types. Do not edit manually."
	schema.Type = "object"
	schema.Types = nil

	events, err := readReconciliationEvents(root)
	if err != nil {
		return nil, err
	}

	addConstraints(schema, events)

	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode deployment schema: %w", err)
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("read generated deployment schema: %w", err)
	}

	versionJSON, err := json.Marshal(version)
	if err != nil {
		return nil, fmt.Errorf("encode Doco-CD version: %w", err)
	}

	document["x-doco-cd-version"] = versionJSON

	data, err = json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode versioned deployment schema: %w", err)
	}

	return append(data, '\n'), nil
}

func docoCDVersion(root string) (string, error) {
	if version := strings.TrimSpace(os.Getenv("DOCO_CD_VERSION")); version != "" {
		return version, nil
	}

	command := exec.Command("git", "describe", "--tags", "--always", "--dirty")
	command.Dir = root

	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("determine Doco-CD version (set DOCO_CD_VERSION to override): %w", err)
	}

	version := strings.TrimSpace(string(output))
	if version == "" {
		return "", fmt.Errorf("determine Doco-CD version: git describe returned an empty version")
	}

	return version, nil
}

// describe adjusts JSON serialization schemas for YAML authoring, where omitted
// fields inherit defaults and null leaves a setting unchanged.
func describe(schema *jsonschema.Schema, typ reflect.Type, value reflect.Value, descriptions fieldDescriptions) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()

		if value.IsNil() {
			value = reflect.Zero(typ)
		} else {
			value = value.Elem()
		}
	}

	switch typ.Kind() {
	case reflect.Struct:
		schema.Type = ""
		schema.Types = []string{"object", "null"}
		schema.Required = nil

		for field := range typ.NumField() {
			member := typ.Field(field)

			name, _, _ := strings.Cut(member.Tag.Get("json"), ",")
			if !member.IsExported() || name == "-" {
				continue
			}

			if name == "" {
				return fmt.Errorf("%s.%s has no public JSON name", typ, member.Name)
			}

			property := schema.Properties[name]
			if property == nil {
				return fmt.Errorf("schema property missing for %s.%s", typ, member.Name)
			}

			description := descriptions[typ.PkgPath()+"."+typ.Name()][member.Name]
			if description == "" {
				return fmt.Errorf("field description missing for %s.%s", typ, member.Name)
			}

			if err := describe(property, member.Type, value.Field(field), descriptions); err != nil {
				return err
			}

			property.Description = description
			property.Deprecated = strings.HasPrefix(description, "Deprecated:")

			if _, hasDefault := member.Tag.Lookup("default"); hasDefault {
				data, err := json.Marshal(value.Field(field).Interface())
				if err != nil {
					return fmt.Errorf("encode default for %s.%s: %w", typ, member.Name, err)
				}

				property.Default = data
			}
		}

		switch typ {
		case reflect.TypeFor[deploy.DestroyConfig](), reflect.TypeFor[deploy.AutoDiscoveryConfig](), reflect.TypeFor[deploy.ReconciliationConfig]():
			schema.Types = []string{"boolean", "object", "null"}
		case reflect.TypeFor[secrettypes.ExternalSecretRef]():
			schema.Types = []string{"string", "number", "boolean", "object"}
			schema.MinLength = new(1)
			schema.Required = []string{"store_ref"}
			schema.Properties["store_ref"].Types = nil
			schema.Properties["store_ref"].Type = "string"
			schema.Properties["store_ref"].MinLength = new(1)
		}
	case reflect.Slice, reflect.Array:
		if err := describe(schema.Items, typ.Elem(), reflect.Zero(typ.Elem()), descriptions); err != nil {
			return err
		}
	case reflect.Map:
		schema.Type = ""
		schema.Types = []string{"object", "null"}

		if err := describe(schema.AdditionalProperties, typ.Elem(), reflect.Zero(typ.Elem()), descriptions); err != nil {
			return err
		}
	case reflect.String:
		// The YAML loader converts scalar numbers and booleans to Go strings.
		schema.Type = ""
		schema.Types = []string{"string", "number", "boolean", "null"}
	case reflect.Bool:
		schema.Type = ""
		schema.Types = []string{"boolean", "null"}
	case reflect.Int:
		schema.Type = ""
		schema.Types = []string{"integer", "null"}
	}

	return nil
}

func addConstraints(schema *jsonschema.Schema, supportedEvents []string) {
	source := schema.Properties["source"]
	source.Types = []string{"string", "null"}
	source.Pattern = caseInsensitiveChoices(string(config.SourceTypeGit), string(config.SourceTypeOCI), "")
	source.Examples = []any{config.SourceTypeGit, config.SourceTypeOCI}

	schema.Properties["compose_files"].MinItems = new(1)
	schema.Properties["git_depth"].Minimum = new(0.0)
	schema.Properties["auto_discovery"].Properties["depth"].Minimum = new(0.0)

	for _, field := range []string{"config_retention", "secret_retention"} {
		schema.Properties["swarm"].Properties[field].Minimum = new(-1.0)
	}

	reconciliation := schema.Properties["reconciliation"]
	for _, field := range []string{"restart_timeout", "restart_limit", "restart_window"} {
		reconciliation.Properties[field].Minimum = new(0.0)
	}

	events := reconciliation.Properties["events"].Items
	events.Types = nil
	events.Type = "string"

	supportedEvents = append(supportedEvents, "remove", "delete")
	events.Pattern = caseInsensitiveChoices(supportedEvents...)

	for _, event := range supportedEvents {
		events.Examples = append(events.Examples, event)
	}

	// Only an explicitly selected OCI source constrains the artifact version;
	// an override can inherit its source from a different configuration.
	schema.If = &jsonschema.Schema{
		Required: []string{"source"},
		Properties: map[string]*jsonschema.Schema{
			"source": {Type: "string", Pattern: caseInsensitiveChoices(string(config.SourceTypeOCI))},
		},
	}
	schema.Then = &jsonschema.Schema{
		Properties: map[string]*jsonschema.Schema{
			"version": {Types: []string{"string", "null"}, Pattern: `^\s*(` + regexp.QuoteMeta(config.OciArtifactLayoutV1) + `)?\s*$`},
		},
	}
}

// JSON Schema patterns use ECMAScript syntax, so Go's (?i) flag is not portable.
func caseInsensitiveChoices(choices ...string) string {
	patterns := make([]string, 0, len(choices))

	for _, choice := range choices {
		var pattern strings.Builder

		for _, character := range choice {
			pattern.WriteString("[")
			pattern.WriteString(strings.ToLower(string(character)))
			pattern.WriteString(strings.ToUpper(string(character)))
			pattern.WriteString("]")
		}

		patterns = append(patterns, pattern.String())
	}

	return `^\s*(` + strings.Join(patterns, "|") + `)\s*$`
}
