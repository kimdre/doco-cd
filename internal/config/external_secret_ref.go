package config

import (
	"encoding/json"
	"errors"

	"go.yaml.in/yaml/v3"
)

// ExternalSecretRef represents one external secret reference in deploy config.
// It supports legacy scalar references (for existing providers) and structured
// object references used by webhook stores.
type ExternalSecretRef struct {
	LegacyRef string                 `json:"-"`
	StoreRef  string                 `json:"storeRef,omitempty"`
	RemoteRef map[string]interface{} `json:"remoteRef,omitempty"`
}

func (r *ExternalSecretRef) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var v string
		if err := node.Decode(&v); err != nil {
			return err
		}

		r.LegacyRef = v
		r.StoreRef = ""
		r.RemoteRef = nil

		return nil
	case yaml.MappingNode:
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
func (r ExternalSecretRef) EncodedReference() (string, error) {
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
