package secrettypes

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/compose-spec/compose-go/v2/template"
	"go.yaml.in/yaml/v4"
)

var (
	// simpleBracedVariable matches an unguarded braced variable, for example ${SECRET_ID}.
	simpleBracedVariable = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)
	// simpleVariable matches an unguarded variable, for example $SECRET_ID.
	simpleVariable = regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*`)
)

// ExternalSecretRef represents one external secret reference in deploy config.
// It supports legacy scalar references (for existing providers like Bitwarden SM,
// 1Password, AWS Secrets Manager etc.) and structured object references used by
// the webhook provider's store-based model.
type ExternalSecretRef struct {
	// LegacyRef holds the raw string value when the reference is written as a
	// plain scalar in YAML (e.g. `DB_PASSWORD: 138e3a97-ed58-431c-b366-b35500663411`).
	// Used by all non-webhook secret providers. Empty for structured refs.
	LegacyRef string `json:"-"`

	// StoreRef is the name of the global webhook secret store to use, as defined
	// in the store YAML file (e.g. `store_ref: bitwarden-login`).
	// Used exclusively by the webhook provider.
	StoreRef string `json:"store_ref,omitempty"`

	// RemoteRef contains the dynamic key/value pairs that are substituted into
	// the store's URL, headers, body and json_path templates at resolution time
	// (e.g. `key`, `property`, or any custom field the store templates reference).
	// Used exclusively by the webhook provider.
	RemoteRef map[string]any `json:"remote_ref,omitempty"`
}

func (r *ExternalSecretRef) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		// Legacy scalar form used by non-webhook providers:
		//   DB_PASSWORD: 138e3a97-ed58-431c-b366-b35500663411
		var v string
		if err := node.Decode(&v); err != nil {
			return err
		}

		if v == "" {
			return errors.New("invalid external secret reference: string must not be empty")
		}

		r.LegacyRef = v
		r.StoreRef = ""
		r.RemoteRef = nil

		return nil
	case yaml.MappingNode:
		if hasYAMLKey(node, "storeRef") || hasYAMLKey(node, "remoteRef") {
			return errors.New("invalid external secret reference: use snake_case keys store_ref and remote_ref")
		}

		// Structured object form used by the webhook provider:
		//   DB_PASSWORD:
		//     store_ref: bitwarden-login
		//     remote_ref:
		//       key: 138e3a97-ed58-431c-b366-b35500663411
		//       property: password
		type ref struct {
			StoreRef  string         `yaml:"store_ref"`
			RemoteRef map[string]any `yaml:"remote_ref"`
		}

		var v ref
		if err := node.Decode(&v); err != nil {
			return err
		}

		r.LegacyRef = ""
		r.StoreRef = v.StoreRef
		r.RemoteRef = v.RemoteRef

		return nil
	default:
		return errors.New("invalid external secret reference: expected string or object")
	}
}

// EncodedReference returns the string representation sent to provider implementations.
// Legacy refs are returned as-is; structured refs are encoded as JSON.
func (r *ExternalSecretRef) EncodedReference() (string, error) {
	if r.LegacyRef != "" {
		return r.LegacyRef, nil
	}

	if r.StoreRef == "" && r.RemoteRef == nil {
		return "", errors.New("invalid external secret reference: reference is empty")
	}

	b, err := json.Marshal(struct {
		StoreRef  string         `json:"store_ref"`
		RemoteRef map[string]any `json:"remote_ref"`
	}{
		StoreRef:  r.StoreRef,
		RemoteRef: r.RemoteRef,
	})
	if err != nil {
		return "", err
	}

	return string(b), nil
}

func hasYAMLKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}

	return false
}

// EncodeExternalSecretRefs converts typed references to provider input values.
func EncodeExternalSecretRefs(in map[string]ExternalSecretRef) (map[string]string, error) {
	out := make(map[string]string, len(in))

	for envName, ref := range in {
		encoded, err := ref.EncodedReference()
		if err != nil {
			return nil, err
		}

		out[envName] = encoded
	}

	return out, nil
}

// InterpolateExternalSecretRefs expands Compose-style variables in legacy
// external secret references when enabled. Structured references are preserved
// unchanged and the input map is never mutated.
func InterpolateExternalSecretRefs(in map[string]ExternalSecretRef, enabled bool) (map[string]ExternalSecretRef, error) {
	if !enabled {
		return in, nil
	}

	out := make(map[string]ExternalSecretRef, len(in))

	for envName, ref := range in {
		if ref.LegacyRef == "" {
			out[envName] = ref
			continue
		}

		value, err := template.SubstituteWithOptions(requireExternalSecretVariables(ref.LegacyRef), os.LookupEnv, template.WithoutLogging)
		if err != nil {
			return nil, fmt.Errorf("interpolate external secret %q: %w", envName, err)
		}

		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("interpolate external secret %q: reference is empty after interpolation", envName)
		}

		ref.LegacyRef = value
		out[envName] = ref
	}

	return out, nil
}

// requireExternalSecretVariables rewrites unguarded variables as requiredCompose variables
// while preserving defaults, presence operators, and escapes.
func requireExternalSecretVariables(ref string) string {
	const escapedDollar = "\x00"

	ref = strings.ReplaceAll(ref, "$$", escapedDollar)
	ref = simpleBracedVariable.ReplaceAllStringFunc(ref, func(variable string) string {
		return variable[:len(variable)-1] + "?}"
	})
	ref = simpleVariable.ReplaceAllStringFunc(ref, func(variable string) string {
		return "${" + variable[1:] + "?}"
	})

	return strings.ReplaceAll(ref, escapedDollar, "$$")
}
