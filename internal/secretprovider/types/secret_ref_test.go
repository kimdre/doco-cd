package secrettypes

import (
	"reflect"
	"testing"

	"go.yaml.in/yaml/v4"
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
					RemoteRef: map[string]any{"key": "test", "property": "password"},
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
			RemoteRef: map[string]any{
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

func TestHashResolvedExternalSecretValues(t *testing.T) {
	t.Parallel()

	refs := map[string]ExternalSecretRef{
		"DB_PASSWORD": {LegacyRef: "db/password"},
		"API_KEY":     {LegacyRef: "api/key"},
	}

	env1 := map[string]string{
		"DB_PASSWORD": "secret-a",
		"API_KEY":     "secret-b",
	}
	env2 := map[string]string{
		"API_KEY":     "secret-b",
		"DB_PASSWORD": "secret-a",
	}

	h1, err := HashResolvedExternalSecretValues(refs, env1)
	if err != nil {
		t.Fatalf("unexpected hash error: %v", err)
	}

	h2, err := HashResolvedExternalSecretValues(refs, env2)
	if err != nil {
		t.Fatalf("unexpected hash error: %v", err)
	}

	if h1 == "" || h2 == "" {
		t.Fatalf("expected non-empty hash")
	}

	if h1 != h2 {
		t.Fatalf("expected stable hash regardless of map order, got %q vs %q", h1, h2)
	}
}
