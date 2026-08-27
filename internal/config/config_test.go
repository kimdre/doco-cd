package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/encryption"
)

func TestNormalizeSourceType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   SourceType
		want SourceType
	}{
		{name: "empty defaults to git", in: "", want: SourceTypeGit},
		{name: "trims whitespace", in: "  OCI  ", want: SourceTypeOCI},
		{name: "lowercases", in: "GiT", want: SourceTypeGit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := NormalizeSourceType(tt.in); got != tt.want {
				t.Fatalf("NormalizeSourceType(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidateSourceType(t *testing.T) {
	t.Parallel()

	if err := ValidateSourceType(SourceTypeGit); err != nil {
		t.Fatalf("ValidateSourceType(git) = %v", err)
	}

	if err := ValidateSourceType(SourceTypeOCI); err != nil {
		t.Fatalf("ValidateSourceType(oci) = %v", err)
	}

	if err := ValidateSourceType(""); err != nil {
		t.Fatalf("ValidateSourceType(empty) = %v", err)
	}

	if err := ValidateSourceType("docker"); err == nil {
		t.Fatal("ValidateSourceType(invalid) = nil, want error")
	}
}

func TestHttpUrlValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   HttpUrl
		want bool
	}{
		{name: "valid https", in: "https://example.com/api", want: true},
		{name: "valid http", in: "http://example.com", want: true},
		{name: "empty is allowed", in: "", want: true},
		{name: "invalid scheme", in: "ftp://example.com", want: false},
		{name: "missing host", in: "https://", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.in.Validate()
			if tt.want && err != nil {
				t.Fatalf("HttpUrl.Validate() = %v, want nil", err)
			}

			if !tt.want && err == nil {
				t.Fatal("HttpUrl.Validate() = nil, want error")
			}
		})
	}
}

func TestGitUrlValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   GitUrl
		want bool
	}{
		{name: "valid https", in: "https://example.com/repo.git", want: true},
		{name: "valid ssh", in: "ssh://git@example.com/repo.git", want: true},
		{name: "valid scp style", in: "git@example.com:owner/repo.git", want: true},
		{name: "empty is allowed", in: "", want: true},
		{name: "invalid scheme", in: "ftp://example.com/repo.git", want: false},
		{name: "missing host", in: "ssh:///repo.git", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.in.Validate()
			if tt.want && err != nil {
				t.Fatalf("GitUrl.Validate() = %v, want nil", err)
			}

			if !tt.want && err == nil {
				t.Fatal("GitUrl.Validate() = nil, want error")
			}
		})
	}
}

func TestGitURLValidationErrorDoesNotExposeCredentials(t *testing.T) {
	t.Parallel()

	const secret = "sentinel-secret"

	err := GitUrl("ftp://user:" + secret + "@example.com/repo.git").Validate()
	if err == nil {
		t.Fatal("GitUrl.Validate() = nil, want error")
	}

	if strings.Contains(err.Error(), secret) {
		t.Fatalf("GitUrl.Validate() exposed credentials: %v", err)
	}
}

func TestOciUrlValidateAndTag(t *testing.T) {
	t.Parallel()

	valid := OciUrl("ghcr.io/example/app:v1.2.3")
	if err := valid.Validate(); err != nil {
		t.Fatalf("OciUrl.Validate() = %v", err)
	}

	if got := valid.Tag(); got != "v1.2.3" {
		t.Fatalf("OciUrl.Tag() = %q, want %q", got, "v1.2.3")
	}

	if err := OciUrl("").Validate(); err != nil {
		t.Fatalf("OciUrl.Validate(empty) = %v", err)
	}

	if err := OciUrl("not a ref").Validate(); err == nil {
		t.Fatal("OciUrl.Validate(invalid) = nil, want error")
	}

	if got := OciUrl("not a ref").Tag(); got != "" {
		t.Fatalf("OciUrl.Tag(invalid) = %q, want empty string", got)
	}
}

func TestValidationAdaptersRejectWrongType(t *testing.T) {
	t.Parallel()

	if err := validateHttpUrl(123, ""); err == nil {
		t.Fatal("validateHttpUrl() = nil, want error")
	}

	if err := validateGitUrl(123, ""); err == nil {
		t.Fatal("validateGitUrl() = nil, want error")
	}

	if err := validateOciUrl(123, ""); err == nil {
		t.Fatal("validateOciUrl() = nil, want error")
	}
}

func TestNormalizeOciTrustPolicy(t *testing.T) {
	t.Parallel()

	t.Run("enabled policy is normalized", func(t *testing.T) {
		t.Parallel()

		policy := OciTrustPolicy{
			Enabled: true,
			KeylessIdentities: []OciKeylessIdentity{{
				Issuer:        " https://issuer.example ",
				Subject:       " subject ",
				SubjectRegexp: " ^subject$ ",
			}},
			PublicKeys: []string{" key-one ", "", " key-two "},
			IgnoreTlog: true,
		}

		normalized := NormalizeOciTrustPolicy(policy)
		if got := normalized.KeylessIdentities[0]; got.Issuer != "https://issuer.example" || got.Subject != "subject" || got.SubjectRegexp != "^subject$" {
			t.Fatalf("NormalizeOciTrustPolicy() trimmed fields incorrectly: %#v", got)
		}

		if len(normalized.PublicKeys) != 2 || normalized.PublicKeys[0] != "key-one" || normalized.PublicKeys[1] != "key-two" {
			t.Fatalf("NormalizeOciTrustPolicy() public keys = %#v, want trimmed non-empty keys", normalized.PublicKeys)
		}
	})

	t.Run("disabled policy is left alone", func(t *testing.T) {
		t.Parallel()

		policy := OciTrustPolicy{Enabled: false, PublicKeys: []string{" keep "}}

		normalized := NormalizeOciTrustPolicy(policy)
		if normalized.PublicKeys[0] != " keep " {
			t.Fatalf("NormalizeOciTrustPolicy() = %#v, want unchanged disabled policy", normalized)
		}
	})
}

func TestEffectiveOciTrustPolicy(t *testing.T) {
	t.Parallel()

	t.Run("override can enable disabled verification", func(t *testing.T) {
		t.Parallel()

		effective := EffectiveOciTrustPolicy(
			OciTrustPolicy{Enabled: false},
			OciTrustPolicyOverride{Verify: func() *bool { v := true; return &v }()},
		)

		if !effective.Enabled {
			t.Fatal("expected override Verify=true to enable verification")
		}
	})

	t.Run("override merges trust data", func(t *testing.T) {
		t.Parallel()

		effective := EffectiveOciTrustPolicy(
			OciTrustPolicy{
				Enabled:    true,
				IgnoreTlog: false,
			},
			OciTrustPolicyOverride{
				Verify:            new(bool),
				KeylessIdentities: []OciKeylessIdentity{{Issuer: "override"}},
				PublicKeys:        []string{"override-key"},
				IgnoreTlog:        func() *bool { v := true; return &v }(),
			},
		)

		if !effective.Enabled {
			t.Fatal("expected enabled policy to stay enabled")
		}

		if len(effective.KeylessIdentities) != 1 || effective.KeylessIdentities[0].Issuer != "override" {
			t.Fatalf("EffectiveOciTrustPolicy() keyless identities = %#v", effective.KeylessIdentities)
		}

		if len(effective.PublicKeys) != 1 || effective.PublicKeys[0] != "override-key" {
			t.Fatalf("EffectiveOciTrustPolicy() public keys = %#v", effective.PublicKeys)
		}

		if !effective.IgnoreTlog {
			t.Fatal("expected override IgnoreTlog=true to win")
		}
	})
}

func TestLoadFileBasedEnvVars(t *testing.T) {
	tests := []struct {
		name       string
		mapping    EnvVarFileMapping
		want       string
		wantErr    error
		setup      func(t *testing.T)
		setEnvFile string
	}{
		{
			name: "loads plain file content",
			mapping: EnvVarFileMapping{
				EnvName:   "API_SECRET",
				EnvValue:  func() *string { v := ""; return &v }(),
				FileValue: func() *string { v := "  plain-secret  "; return &v }(),
			},
			want: "plain-secret",
		},
		{
			name: "rejects both set",
			mapping: EnvVarFileMapping{
				EnvName:   "API_SECRET",
				EnvValue:  func() *string { v := "value"; return &v }(),
				FileValue: func() *string { v := "file"; return &v }(),
			},
			wantErr: ErrBothSecretsSet,
		},
		{
			name: "rejects missing value when unset is not allowed",
			mapping: EnvVarFileMapping{
				EnvName: "API_SECRET",
			},
			wantErr: ErrBothSecretsNotSet,
		},
		{
			name: "decrypts encrypted file content",
			mapping: EnvVarFileMapping{
				EnvName:  "API_SECRET",
				EnvValue: func() *string { v := ""; return &v }(),
			},
			want: "this.is.encrypted: \"yes\"",
			setup: func(t *testing.T) {
				t.Helper()
				encryption.SetupAgeKeyEnvVar(t)
			},
			setEnvFile: filepath.Join("..", "encryption", "testdata", "encrypted.yaml"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("API_SECRET_FILE", "")

			if tt.setup != nil {
				tt.setup(t)
			}

			mapping := tt.mapping
			mappings := []EnvVarFileMapping{mapping}

			if tt.setEnvFile != "" {
				content, err := os.ReadFile(tt.setEnvFile)
				if err != nil {
					t.Fatalf("ReadFile(%s) = %v", tt.setEnvFile, err)
				}

				mappings[0].FileValue = func() *string { v := string(content); return &v }()

				t.Setenv("API_SECRET_FILE", tt.setEnvFile)
			}

			err := LoadFileBasedEnvVars(&mappings)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("LoadFileBasedEnvVars() = nil, want %v", tt.wantErr)
				}

				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("LoadFileBasedEnvVars() = %v, want %v", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("LoadFileBasedEnvVars() = %v", err)
			}

			if got := *mappings[0].EnvValue; got != tt.want {
				t.Fatalf("LoadFileBasedEnvVars() env value = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseConfigFromEnv(t *testing.T) {
	type sampleConfig struct {
		Name string `env:"SAMPLE_NAME,notEmpty" validate:"required"`
	}

	t.Setenv("SAMPLE_NAME", "doco")

	var cfg sampleConfig
	if err := ParseConfigFromEnv(&cfg, &[]EnvVarFileMapping{}); err != nil {
		t.Fatalf("ParseConfigFromEnv() = %v", err)
	}

	if cfg.Name != "doco" {
		t.Fatalf("ParseConfigFromEnv() name = %q, want %q", cfg.Name, "doco")
	}
}
