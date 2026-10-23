package secrettypes

import (
	"maps"
	"strings"
	"testing"
)

func TestNormalizeEnvVarName(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		secret string
		want   string
	}{
		{name: "upper case", secret: "db_password", want: "DB_PASSWORD"},
		{name: "dash and dot", secret: "db-password.main", want: "DB_PASSWORD_MAIN"},
		{name: "slash", secret: "app/db/password", want: "APP_DB_PASSWORD"},
		{name: "prefix", prefix: "app_", secret: "token", want: "APP_TOKEN"},
		{name: "leading digit", secret: "1password", want: "_1PASSWORD"},
		{name: "digit after prefix", prefix: "N", secret: "1", want: "N1"},
		{name: "no letter or digit", secret: "---", want: ""},
		{name: "empty", want: ""},
		{name: "non ascii letters", secret: "pässword", want: "P_SSWORD"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeEnvVarName(tt.prefix, tt.secret); got != tt.want {
				t.Errorf("NormalizeEnvVarName(%q, %q) = %q, want %q", tt.prefix, tt.secret, got, tt.want)
			}
		})
	}
}

func TestMergeBulkSecrets(t *testing.T) {
	source := ExternalSecretsPath{Path: "apps/", Prefix: "APP_"}

	t.Run("adds normalized names", func(t *testing.T) {
		dst := map[string]string{"EXISTING": "x"}

		err := MergeBulkSecrets(dst, source, map[string]string{"db-password": "one", "api/key": "two"})
		if err != nil {
			t.Fatalf("MergeBulkSecrets() error = %v", err)
		}

		want := map[string]string{"EXISTING": "x", "APP_DB_PASSWORD": "one", "APP_API_KEY": "two"}
		if !maps.Equal(dst, want) {
			t.Errorf("dst = %v, want %v", dst, want)
		}
	})

	t.Run("collision inside one path", func(t *testing.T) {
		err := MergeBulkSecrets(map[string]string{}, source, map[string]string{"db-password": "one", "db_password": "two"})
		if err == nil {
			t.Fatal("expected a collision error")
		}

		if strings.Contains(err.Error(), "one") || strings.Contains(err.Error(), "two") {
			t.Errorf("error must not contain secret values: %v", err)
		}
	})

	t.Run("collision with existing variable", func(t *testing.T) {
		dst := map[string]string{"APP_TOKEN": "old"}

		if err := MergeBulkSecrets(dst, source, map[string]string{"token": "new"}); err == nil {
			t.Fatal("expected a collision error")
		}

		if dst["APP_TOKEN"] != "old" {
			t.Errorf("existing value was changed: %q", dst["APP_TOKEN"])
		}
	})

	t.Run("name cannot be converted", func(t *testing.T) {
		if err := MergeBulkSecrets(map[string]string{}, ExternalSecretsPath{Path: "p"}, map[string]string{"--": "v"}); err == nil {
			t.Fatal("expected a conversion error")
		}
	})
}

func TestExternalSecretsPathValidate(t *testing.T) {
	if err := (ExternalSecretsPath{Path: "apps/"}).Validate(); err != nil {
		t.Errorf("Validate() error = %v", err)
	}

	for _, path := range []string{"", "   "} {
		if err := (ExternalSecretsPath{Path: path}).Validate(); err == nil {
			t.Errorf("Validate() with path %q: expected an error", path)
		}
	}
}

func TestInterpolateExternalSecretsPaths(t *testing.T) {
	t.Setenv("BULK_STAGE", "lab")

	in := []ExternalSecretsPath{{Path: "apps/${BULK_STAGE}/", Prefix: "P_", Recursive: true}}

	t.Run("disabled keeps input", func(t *testing.T) {
		out, err := InterpolateExternalSecretsPaths(in, false)
		if err != nil {
			t.Fatal(err)
		}

		if out[0].Path != "apps/${BULK_STAGE}/" {
			t.Errorf("path = %q", out[0].Path)
		}
	})

	t.Run("enabled expands and does not mutate input", func(t *testing.T) {
		out, err := InterpolateExternalSecretsPaths(in, true)
		if err != nil {
			t.Fatal(err)
		}

		if out[0].Path != "apps/lab/" || out[0].Prefix != "P_" || !out[0].Recursive {
			t.Errorf("unexpected entry: %+v", out[0])
		}

		if in[0].Path != "apps/${BULK_STAGE}/" {
			t.Errorf("input was mutated: %q", in[0].Path)
		}
	})

	t.Run("missing variable fails", func(t *testing.T) {
		_, err := InterpolateExternalSecretsPaths([]ExternalSecretsPath{{Path: "apps/${BULK_MISSING_VAR}/"}}, true)
		if err == nil {
			t.Fatal("expected an error for an unset variable")
		}
	})
}
