package config

import (
	"encoding/json"
	"errors"

	"go.yaml.in/yaml/v3"
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
	// in the store YAML file (e.g. `storeRef: bitwarden-login`).
	// Used exclusively by the webhook provider.
	StoreRef string `json:"storeRef,omitempty"`

	// RemoteRef contains the dynamic key/value pairs that are substituted into
	// the store's URL, headers, body and jsonPath templates at resolution time
	// (e.g. `key`, `property`, or any custom field the store templates reference).
	// Used exclusively by the webhook provider.
	RemoteRef map[string]interface{} `json:"remoteRef,omitempty"`
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

		r.LegacyRef = v
		r.StoreRef = ""
		r.RemoteRef = nil

		return nil
	case yaml.MappingNode:
		// Structured object form used by the webhook provider:
		//   DB_PASSWORD:
		//     storeRef: bitwarden-login
		//     remoteRef:
		//       key: 138e3a97-ed58-431c-b366-b35500663411
		//       property: password
		type ref struct {
			StoreRef  string                 `yaml:"storeRef"`
			RemoteRef map[string]interface{} `yaml:"remoteRef"`
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

	b, err := json.Marshal(struct {
		StoreRef  string                 `json:"storeRef"`
		RemoteRef map[string]interface{} `json:"remoteRef"`
	}{
		StoreRef:  r.StoreRef,
		RemoteRef: r.RemoteRef,
	})
	if err != nil {
		return "", err
	}

	return string(b), nil
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
