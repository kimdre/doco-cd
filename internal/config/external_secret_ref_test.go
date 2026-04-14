package config

import (
	"reflect"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestExternalSecretRef_UnmarshalYAML(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want map[string]ExternalSecretRef
	}{
		{
			name: "legacy scalar",
			yaml: "external_secrets:\n  DB_PASSWORD: abc\n",
			want: map[string]ExternalSecretRef{
				"DB_PASSWORD": {LegacyRef: "abc"},
			},
		},
		{
			name: "structured ref snake_case",
			yaml: "external_secrets:\n  DB_PASSWORD:\n    store_ref: bitwarden-login\n    remote_ref:\n      key: test\n      property: password\n",
			want: map[string]ExternalSecretRef{
				"DB_PASSWORD": {
					StoreRef:  "bitwarden-login",
					RemoteRef: map[string]interface{}{"key": "test", "property": "password"},
				},
			},
		},
		{
			name: "structured ref legacy camelCase",
			yaml: "external_secrets:\n  DB_PASSWORD:\n    storeRef: bitwarden-login\n    remoteRef:\n      key: test\n      property: password\n",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var data struct {
				ExternalSecrets map[string]ExternalSecretRef `yaml:"external_secrets"`
			}

			err := yaml.Unmarshal([]byte(tc.yaml), &data)
			if tc.want == nil {
				if err == nil {
					t.Fatalf("expected unmarshal error")
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected unmarshal error: %v", err)
			}

			if !reflect.DeepEqual(tc.want, data.ExternalSecrets) {
				t.Fatalf("got %#v, want %#v", data.ExternalSecrets, tc.want)
			}
		})
	}
}

func TestEncodeExternalSecretRefs(t *testing.T) {
	t.Parallel()

	in := map[string]ExternalSecretRef{
		"LEGACY": {LegacyRef: "plain-ref"},
		"JSON": {
			StoreRef: "store-1",
			RemoteRef: map[string]interface{}{
				"key": "abc",
			},
		},
	}

	got, err := EncodeExternalSecretRefs(in)
	if err != nil {
		t.Fatalf("unexpected encode error: %v", err)
	}

	if got["LEGACY"] != "plain-ref" {
		t.Fatalf("got %q for legacy ref", got["LEGACY"])
	}

	if got["JSON"] == "" || got["JSON"] == "plain-ref" {
		t.Fatalf("expected JSON encoded structured ref, got %q", got["JSON"])
	}
}
