package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/defaults"
	"github.com/kimdre/doco-cd/internal/config/deploy"
)

func TestGeneratedSchema(t *testing.T) {
	t.Parallel()

	data, err := generate("../..")
	if err != nil {
		t.Fatal(err)
	}

	repeated, err := generate("../..")
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(data, repeated) {
		t.Fatal("deployment schema generation must be deterministic")
	}

	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}

	if _, err := schema.Resolve(nil); err != nil {
		t.Fatalf("resolve generated schema: %v", err)
	}

	if schema.ID != schemaURL || schema.Schema != "http://json-schema.org/draft-07/schema#" {
		t.Fatal("unexpected schema identifier or dialect")
	}

	var defaultConfig deploy.Config
	if err := defaults.Set(&defaultConfig); err != nil {
		t.Fatal(err)
	}

	checkFields(t, &schema, reflect.TypeFor[deploy.Config](), reflect.ValueOf(defaultConfig))

	if !schema.Properties["destroy"].Properties["remove_dir"].Deprecated {
		t.Error("destroy.remove_dir must be marked deprecated")
	}
}

func checkFields(t *testing.T, schema *jsonschema.Schema, typ reflect.Type, value reflect.Value) {
	t.Helper()

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
		publicFields := 0

		for field := range typ.NumField() {
			member := typ.Field(field)

			name, _, _ := strings.Cut(member.Tag.Get("json"), ",")
			if !member.IsExported() || name == "-" {
				continue
			}

			publicFields++

			property := schema.Properties[name]
			if property == nil {
				t.Fatalf("property missing for %s.%s", typ, member.Name)
			}

			if property.Description == "" {
				t.Errorf("description missing for %s.%s", typ, member.Name)
			}

			if _, hasDefault := member.Tag.Lookup("default"); hasDefault {
				expected, err := json.Marshal(value.Field(field).Interface())
				if err != nil {
					t.Fatal(err)
				}

				var actual bytes.Buffer
				if err := json.Compact(&actual, property.Default); err != nil {
					t.Fatal(err)
				}

				if !bytes.Equal(actual.Bytes(), expected) {
					t.Errorf("default for %s.%s: got %s, want %s", typ, member.Name, property.Default, expected)
				}
			}

			checkFields(t, property, member.Type, value.Field(field))
		}

		if len(schema.Properties) != publicFields {
			t.Errorf("%s: schema has %d properties, want %d public fields", typ, len(schema.Properties), publicFields)
		}
	case reflect.Slice, reflect.Array:
		checkFields(t, schema.Items, typ.Elem(), reflect.Zero(typ.Elem()))
	case reflect.Map:
		checkFields(t, schema.AdditionalProperties, typ.Elem(), reflect.Zero(typ.Elem()))
	}
}

func TestSchemaAuthoring(t *testing.T) {
	t.Parallel()

	schema := resolvedSchema(t)

	cases := []struct {
		name  string
		yaml  string
		valid bool
	}{
		{"minimal deployment", "name: app", true},
		{"partial override", "environment:\n  LOG_LEVEL: debug", true},
		{"empty override", "{}", true},
		{"auto-discovery without name", "auto_discovery: true", true},
		{"auto-discovery options", "auto_discovery: {enabled: true, depth: 2, delete: true, remove_images: false, remove_volumes: false}", true},
		{"destroy shorthand", "name: app\ndestroy: true", true},
		{"destroy options", "destroy: {enabled: true, remove_volumes: false, remove_images: false, remove_dir: true}", true},
		{"reconciliation shorthand", "reconciliation: false", true},
		{"reconciliation options", "reconciliation: {enabled: true, events: [unhealthy, die, remove, ' DELETE '], restart_timeout: 10, restart_signal: SIGTERM, restart_limit: 0, restart_window: 0}", true},
		{"scalar string coercion", "name: 123\nreference: 123\nenvironment: {COUNT: 2, DEBUG: true, EMPTY: null}\nbuild: {args: {VERSION: 12, ENABLED: false}}", true},
		{"deployment options", "name: app\nsource: git\nversion: custom\nrepository_url: https://github.com/example/app.git\nreference: main\nworking_dir: deploy\ncontext: remote\ncompose_files: [compose.yml]\nenv_files: [.env, 'remote:prod.env']\nprofiles: [prod]\nwebhook_filter: '^refs/heads/main$'\nremove_orphans: true\nprune_images: false\nwait_running_jobs: false\nforce_recreate: true\nforce_image_pull: true\ntimeout: 180\ngit_depth: 0\nbuild: {quiet: false, no_cache: true, force_image_pull: true}", true},
		{"legacy secret references", "external_secrets: {PASSWORD: 'op://vault/item/password', NUMERIC: 123, BOOLEAN: true}\nexternal_secrets_files: [secrets.yaml]", true},
		{"structured secret reference", "external_secrets:\n  PASSWORD:\n    store_ref: vault\n    remote_ref: {key: db, property: password, custom: {nested: [1, true, null]}}", true},
		{"swarm settings", "swarm: {enabled: false, config_retention: -1, secret_retention: 0}", true},
		{"OCI settings", "source: ' OCI '\nversion: ' doco.v1 '\noci:\n  verify: true\n  ignore_tlog: false\n  public_keys: [cosign.pub]\n  keyless_identities:\n    - issuer: https://token.actions.githubusercontent.com\n      subject: example\n      subject_regexp: '^https://github.com/example/'", true},
		{"default OCI version", "source: oci\nversion: ''", true},
		{"empty source uses default", "source: ''", true},
		{"null leaves defaults", "name: app\nsource: null\nversion: null\ncompose_files: null\ntimeout: null\nauto_discovery: null\nreconciliation: null\ndestroy: null\nswarm: {enabled: null, config_retention: null}\noci: null", true},
		{"null maps", "environment: null\nexternal_secrets: null\nbuild: {args: null}", true},
		{"null remote ref", "external_secrets: {PASSWORD: {store_ref: vault, remote_ref: null}}", true},
		{"multiple documents", "name: app\n---\nname: db\nreconciliation: true", true},
		{"YAML merge", "environment:\n  BASE: &value hello\n  COPY: *value\nbuild:\n  args:\n    <<: &args {VERSION: 1}\n    DEBUG: true", true},
		{"empty file", "", false},
		{"root array", "- name: app", false},
		{"root scalar", "app", false},
		{"unknown deployment option", "name: app\nworkng_dir: deploy", false},
		{"internal field", "name: app\nInternal: {}", false},
		{"unknown nested option", "build: {no_cach: true}", false},
		{"invalid shorthand", "destroy: []", false},
		{"invalid boolean", "force_image_pull: 1", false},
		{"invalid source", "source: hg", false},
		{"invalid OCI version", "source: oci\nversion: doco.v2", false},
		{"invalid timeout", "timeout: 1.5", false},
		{"negative git depth", "git_depth: -1", false},
		{"negative discovery depth", "auto_discovery: {depth: -1}", false},
		{"invalid retention", "swarm: {config_retention: -2}", false},
		{"empty compose files", "compose_files: []", false},
		{"unknown reconciliation event", "reconciliation: {events: [start]}", false},
		{"empty reconciliation event", "reconciliation: {events: ['']}", false},
		{"negative restart timeout", "reconciliation: {restart_timeout: -1}", false},
		{"negative restart limit", "reconciliation: {restart_limit: -1}", false},
		{"negative restart window", "reconciliation: {restart_window: -1}", false},
		{"empty secret reference", "external_secrets: {PASSWORD: ''}", false},
		{"null secret reference", "external_secrets: {PASSWORD: null}", false},
		{"missing secret store", "external_secrets: {PASSWORD: {remote_ref: {key: db}}}", false},
		{"wrong secret reference keys", "external_secrets: {PASSWORD: {storeRef: vault, remoteRef: {key: db}}}", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateYAML(schema, tc.yaml)
			if (err == nil) != tc.valid {
				t.Fatalf("valid = %v, got %v", tc.valid, err)
			}

			if tc.valid {
				decoder := yaml.NewDecoder(strings.NewReader(tc.yaml))

				for {
					var config deploy.Config

					err := decoder.Decode(&config)
					if errors.Is(err, io.EOF) {
						break
					}

					if err != nil {
						t.Fatalf("schema accepted a shape the deployment loader rejects: %v", err)
					}
				}
			}
		})
	}
}

func TestSchemaExamples(t *testing.T) {
	t.Parallel()

	schema := resolvedSchema(t)
	count := 0

	examples, err := os.OpenRoot("../../examples")
	if err != nil {
		t.Fatal(err)
	}
	defer examples.Close()

	err = fs.WalkDir(examples.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".doco-cd.") ||
			(!strings.HasSuffix(entry.Name(), ".yml") && !strings.HasSuffix(entry.Name(), ".yaml")) {
			return nil
		}

		data, err := examples.ReadFile(path)
		if err != nil {
			return err
		}

		if err := validateYAML(schema, string(data)); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}

		count++

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if count == 0 {
		t.Fatal("no deployment examples found")
	}
}

func TestGenerateMissingSource(t *testing.T) {
	t.Parallel()

	if _, err := generate(t.TempDir()); err == nil {
		t.Fatal("expected missing configuration source to fail generation")
	}
}

func resolvedSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()

	data, err := generate("../..")
	if err != nil {
		t.Fatal(err)
	}

	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}

	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}

	return resolved
}

func validateYAML(schema *jsonschema.Resolved, source string) error {
	decoder := yaml.NewDecoder(strings.NewReader(source))
	documents := 0

	for {
		var document any

		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			if documents == 0 {
				return errors.New("no YAML documents found")
			}

			return nil
		}

		if err != nil {
			return err
		}

		data, err := json.Marshal(document)
		if err != nil {
			return err
		}

		var instance any
		if err := json.Unmarshal(data, &instance); err != nil {
			return err
		}

		if err := schema.Validate(instance); err != nil {
			return err
		}

		documents++
	}
}
