package secrettypes

import (
	"os"
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
		{
			name: "empty scalar",
			yaml: "external_secrets:\n  DB_PASSWORD: \"\"\n",
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

func TestEncodeExternalSecretRefs_Empty(t *testing.T) {
	t.Parallel()

	_, err := EncodeExternalSecretRefs(map[string]ExternalSecretRef{"EMPTY": {}})
	if err == nil {
		t.Fatal("expected encode error")
	}
}

func TestInterpolateExternalSecretRefs(t *testing.T) {
	tests := []struct {
		name      string
		envVars   map[string]string
		unsetVars []string
		input     map[string]ExternalSecretRef
		enabled   bool
		check     func(t *testing.T, got map[string]ExternalSecretRef, in map[string]ExternalSecretRef)
		wantError bool
	}{
		{
			name: "interpolate with defaults",
			envVars: map[string]string{
				"EMPTY":         "",
				"PROJECT_STAGE": "lab",
			},
			unsetVars: []string{"DOCO_CD_TEST_UNSET_STAGE"},
			input: map[string]ExternalSecretRef{
				"DEFAULT":    {LegacyRef: "kv:db-${PROJECT_STAGE:-prod}"},
				"EMPTY":      {LegacyRef: "kv:db-${EMPTY}"},
				"ESCAPED":    {LegacyRef: "kv:db-$${PROJECT_STAGE}"},
				"HARD":       {LegacyRef: "kv:db-${DOCO_CD_TEST_UNSET_STAGE-prod}"},
				"OPTIONAL":   {LegacyRef: "kv:db${DOCO_CD_TEST_UNSET_STAGE:+-${DOCO_CD_TEST_UNSET_STAGE}}"},
				"SET":        {LegacyRef: "kv:db-${PROJECT_STAGE}"},
				"STRUCTURED": {StoreRef: "store", RemoteRef: map[string]any{"key": "${PROJECT_STAGE}"}},
				"UNBRACED":   {LegacyRef: "kv:db-$PROJECT_STAGE"},
			},
			enabled: true,
			check: func(t *testing.T, got map[string]ExternalSecretRef, in map[string]ExternalSecretRef) {
				if got["DEFAULT"].LegacyRef != "kv:db-lab" ||
					got["EMPTY"].LegacyRef != "kv:db-" ||
					got["ESCAPED"].LegacyRef != "kv:db-${PROJECT_STAGE}" ||
					got["HARD"].LegacyRef != "kv:db-prod" ||
					got["OPTIONAL"].LegacyRef != "kv:db" ||
					got["SET"].LegacyRef != "kv:db-lab" ||
					got["UNBRACED"].LegacyRef != "kv:db-lab" {
					t.Fatalf("unexpected interpolated refs: %#v", got)
				}

				if in["DEFAULT"].LegacyRef != "kv:db-${PROJECT_STAGE:-prod}" {
					t.Fatal("input refs were mutated")
				}

				if !reflect.DeepEqual(got["STRUCTURED"], in["STRUCTURED"]) {
					t.Fatal("structured ref was changed")
				}
			},
		},
		{
			name: "default when empty",
			envVars: map[string]string{
				"PROJECT_STAGE": "",
			},
			input: map[string]ExternalSecretRef{
				"DB": {LegacyRef: "kv:db-${PROJECT_STAGE:-prod}"},
			},
			enabled: true,
			check: func(t *testing.T, got map[string]ExternalSecretRef, _ map[string]ExternalSecretRef) {
				if got["DB"].LegacyRef != "kv:db-prod" {
					t.Fatalf("got %q, want %q", got["DB"].LegacyRef, "kv:db-prod")
				}
			},
		},
		{
			name: "reject empty result",
			envVars: map[string]string{
				"SECRET_ID": "",
			},
			input: map[string]ExternalSecretRef{
				"DB": {LegacyRef: "${SECRET_ID}"},
			},
			enabled:   true,
			wantError: true,
		},
		{
			name:      "reject unset variable",
			unsetVars: []string{"DOCO_CD_TEST_SECRET_ID"},
			input: map[string]ExternalSecretRef{
				"DB": {LegacyRef: "kv:db-${DOCO_CD_TEST_SECRET_ID}"},
			},
			enabled:   true,
			wantError: true,
		},
		{
			name: "reject blank result",
			envVars: map[string]string{
				"SECRET_ID": " ",
			},
			input: map[string]ExternalSecretRef{
				"DB": {LegacyRef: "${SECRET_ID}"},
			},
			enabled:   true,
			wantError: true,
		},
		{
			name: "skip when disabled",
			envVars: map[string]string{
				"PROJECT_STAGE": "lab",
			},
			input: map[string]ExternalSecretRef{
				"DB": {LegacyRef: "kv:db-${PROJECT_STAGE:-prod}"},
			},
			enabled: false,
			check: func(t *testing.T, got map[string]ExternalSecretRef, _ map[string]ExternalSecretRef) {
				if got["DB"].LegacyRef != "kv:db-${PROJECT_STAGE:-prod}" {
					t.Fatalf("got %q while interpolation is disabled", got["DB"].LegacyRef)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			for _, name := range tt.unsetVars {
				t.Setenv(name, "")

				if err := os.Unsetenv(name); err != nil {
					t.Fatalf("unset %s: %v", name, err)
				}
			}

			got, err := InterpolateExternalSecretRefs(tt.input, tt.enabled)

			if (err != nil) != tt.wantError {
				if tt.wantError {
					t.Fatalf("expected interpolation error, got nil")
				} else {
					t.Fatalf("unexpected interpolation error: %v", err)
				}
			}

			if !tt.wantError && tt.check != nil {
				tt.check(t, got, tt.input)
			}
		})
	}
}
