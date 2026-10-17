package openbao

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getsops/sops/v3/age"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/encryption"
)

func TestGetConfig_TokenFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		token   string
		file    bool
		content string
		want    string
		wantErr error
	}{
		{name: "static token", token: "static-token", want: "static-token"},
		{name: "file token", file: true, content: "  file-token\n", want: "file-token"},
		{name: "missing credentials", wantErr: config.ErrBothSecretsNotSet},
		{name: "both sources", token: "static-token", file: true, content: "file-token", wantErr: config.ErrBothSecretsSet},
		{name: "both sources with empty file", token: "static-token", file: true, wantErr: config.ErrBothSecretsSet},
		{name: "empty file", file: true, wantErr: errEmptyAccessTokenFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SECRET_PROVIDER_SITE_URL", "https://bao.example.com")
			t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", tc.token)
			t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")

			var path string
			if tc.file {
				path = filepath.Join(t.TempDir(), "token")
				writeTokenFile(t, path, tc.content)
				t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", path)
			}

			cfg, err := GetConfig()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || !errors.Is(err, config.ErrParseConfigFailed) {
					t.Fatalf("GetConfig() error = %v, want %v and ErrParseConfigFailed", err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if cfg.AccessToken != tc.want || cfg.AccessTokenFile != path {
				t.Fatal("GetConfig() did not preserve the resolved token and source path")
			}
		})
	}
}

func TestAccessTokenFile_SOPS(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	path := filepath.Join(t.TempDir(), "token.yaml")
	tokenFile := newAccessTokenFile(path)

	encrypted, err := os.ReadFile(filepath.Join("..", "..", "encryption", "testdata", "encrypted.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	const decrypted = `this.is.encrypted: "yes"`

	writeTokenFile(t, path, "plain-token")

	if got, err := tokenFile.Token(); err != nil || got != "plain-token" {
		t.Fatalf("Token() = %q, %v", got, err)
	}

	writeTokenFile(t, path, string(encrypted))
	// The saved path, not a later environment value, determines the SOPS format.
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "different.env")

	if got, err := tokenFile.Token(); err != nil || got != decrypted {
		t.Fatalf("Token() = %q, %v", got, err)
	}

	damaged := strings.Replace(string(encrypted), "ENC[AES256_GCM,data:", "ENC[AES256_GCM,data:broken", 1)
	writeTokenFile(t, path, damaged)

	if _, err := tokenFile.Token(); err == nil {
		t.Fatal("expected decryption failure")
	}

	writeTokenFile(t, path, "recovered-token")

	if got, err := tokenFile.Token(); err != nil || got != "recovered-token" {
		t.Fatalf("Token() after recovery = %q, %v", got, err)
	}

	writeTokenFile(t, path, string(encrypted))

	if got, err := tokenFile.Token(); err != nil || got != decrypted {
		t.Fatalf("Token() = %q, %v", got, err)
	}

	// Unchanged content must reuse the decrypted token instead of decrypting again.
	t.Setenv(age.SopsAgeKeyFileEnv, filepath.Join(t.TempDir(), "missing-key.txt"))

	if _, err := newAccessTokenFile(path).Token(); err == nil {
		t.Skip("another SOPS key source can decrypt the test file")
	}

	if got, err := tokenFile.Token(); err != nil || got != decrypted {
		t.Fatalf("Token() for unchanged content = %q, %v", got, err)
	}

	writeTokenFile(t, path, damaged)

	if _, err := tokenFile.Token(); err == nil {
		t.Fatal("changed content returned the cached token")
	}
}

func TestGetConfig_TokenFileInvalid(t *testing.T) {
	for _, tc := range []string{"missing", "directory", "whitespace"} {
		t.Run(tc, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")

			switch tc {
			case "directory":
				path = t.TempDir()
			case "whitespace":
				writeTokenFile(t, path, " \n\t")
			}

			t.Setenv("SECRET_PROVIDER_SITE_URL", "https://bao.example.com")
			t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "")
			t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", path)

			if _, err := GetConfig(); !errors.Is(err, config.ErrParseConfigFailed) {
				t.Fatalf("GetConfig() error = %v, want ErrParseConfigFailed", err)
			} else if tc == "whitespace" && !errors.Is(err, errEmptyAccessTokenFile) {
				t.Fatalf("GetConfig() error = %v, want errEmptyAccessTokenFile", err)
			}
		})
	}
}
