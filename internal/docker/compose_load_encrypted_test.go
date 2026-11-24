package docker

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	agekey "filippo.io/age"
	"github.com/getsops/sops/v3/age"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

func useUnrelatedAgeKey(t *testing.T) {
	t.Helper()

	identity, err := agekey.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate unrelated age key: %v", err)
	}

	t.Setenv(age.SopsAgeKeyEnv, identity.String())
}

func TestComposeValidationFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "included.yaml")
	colonPath := filepath.Join(dir, "a: b", "included.yaml")
	cause := errors.New(" additional properties 'sops' not allowed")

	validation := fmt.Errorf("validating %s: %w", path, cause)

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "validation error", err: validation, want: path},
		{name: "wrapped error", err: fmt.Errorf("failed to load: %w", validation), want: path},
		{name: "path contains separator", err: fmt.Errorf("validating %s: %w", colonPath, cause), want: colonPath},
		{name: "validation text only", err: errors.New(validation.Error())},
		{name: "path in unrelated error", err: fmt.Errorf("cannot open validating %s: %w", path, cause)},
		{name: "relative path", err: fmt.Errorf("validating included.yaml: %w", cause)},
		{name: "nil error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := composeValidationFile(tt.err); got != tt.want {
				t.Fatalf("composeValidationFile() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExplainEncryptedComposeFile_Plaintext(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "included.yaml")
	if err := os.WriteFile(path, []byte("services: {}\nsops: {}\n"), filesystem.PermOwner); err != nil {
		t.Fatal(err)
	}

	orig := fmt.Errorf("validating %s: %w", path, errors.New(" additional properties 'sops' not allowed"))
	if got := explainEncryptedComposeFile(orig); got != orig { //nolint:errorlint // The identical error must be returned.
		t.Fatalf("explainEncryptedComposeFile() = %v, want the original error", got)
	}
}

const encryptedIncludeTestChild = "DOCO_CD_TEST_ENCRYPTED_INCLUDE_CHILD"

func runEncryptedIncludeTest(t *testing.T) {
	t.Helper()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// Key detection is cached per process. Isolate each case from other tests and the host's SOPS config.
	filter := "^" + strings.ReplaceAll(t.Name(), "/", "$/^") + "$"
	cmd := exec.CommandContext(t.Context(), executable, "-test.run="+filter) // #nosec G204: run a subtest of this test binary.
	home := t.TempDir()
	cmd.Env = []string{encryptedIncludeTestChild + "=1", "HOME=" + home, "XDG_CONFIG_HOME=" + home}

	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated test failed: %v\n%s", err, output)
	}
}

func TestLoadCompose_IncludedEncryptedFile(t *testing.T) {
	child := os.Getenv(encryptedIncludeTestChild) == "1"
	if !child {
		t.Parallel()
	}

	fixture, err := os.ReadFile("../encryption/testdata/encrypted.yaml")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		content    []byte
		setupKey   func(t *testing.T)
		wantReason string
	}{
		{
			name:       "no key configured",
			content:    fixture,
			wantReason: "no SOPS key is configured",
		},
		{
			name:       "decryptable file",
			content:    fixture,
			setupKey:   encryption.SetupAgeKeyEnvVar,
			wantReason: "it was not decrypted when doco-cd fetched the source",
		},
		{
			name:       "no matching key",
			content:    fixture,
			setupKey:   useUnrelatedAgeKey,
			wantReason: "SOPS could not retrieve the data key",
		},
		{
			name:       "integrity failure",
			content:    append([]byte("unexpected_unencrypted: tampered\n"), fixture...),
			setupKey:   encryption.SetupAgeKeyEnvVar,
			wantReason: "decryption failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !child {
				runEncryptedIncludeTest(t)

				return
			}

			if tt.setupKey != nil {
				tt.setupKey(t)
			}

			dir := t.TempDir()
			includedPath := filepath.Join(dir, "included.yaml")

			if err := os.WriteFile(includedPath, tt.content, filesystem.PermOwner); err != nil {
				t.Fatal(err)
			}

			composePath := filepath.Join(dir, "compose.yaml")
			compose := "include:\n  - included.yaml\nservices:\n  app:\n    image: alpine\n"

			if err := os.WriteFile(composePath, []byte(compose), filesystem.PermOwner); err != nil {
				t.Fatal(err)
			}

			_, err := LoadCompose(t.Context(), nil, dir, dir, "encrypted-include", []string{composePath},
				nil, nil, map[string]string{}, ComposeLoadOptions{})
			if err == nil {
				t.Fatal("LoadCompose() error = nil, want an error")
			}

			if !errors.Is(err, ErrComposeFileEncrypted) {
				t.Fatalf("LoadCompose() error = %v, want ErrComposeFileEncrypted", err)
			}

			msg := err.Error()
			for _, want := range []string{includedPath, tt.wantReason, "additional properties", "'sops'"} {
				if !strings.Contains(msg, want) {
					t.Errorf("LoadCompose() error = %q, want it to contain %q", msg, want)
				}
			}

			got, readErr := os.ReadFile(includedPath) // #nosec G304: path is in the test temp directory.
			if readErr != nil {
				t.Fatal(readErr)
			}

			if !bytes.Equal(got, tt.content) {
				t.Error("LoadCompose() changed the included encrypted file")
			}
		})
	}
}
