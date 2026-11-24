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
	"time"

	agekey "filippo.io/age"
	"github.com/getsops/sops/v3/age"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/source/store"
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

const extendsComposeFile = "services:\n  app:\n    extends:\n      file: %s\n      service: base\n"

func writeTestFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), filesystem.PermOwner); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadCompose_EncryptedExtendsFile(t *testing.T) {
	child := os.Getenv(encryptedIncludeTestChild) == "1"
	if !child {
		t.Parallel()
	}

	fixture, err := os.ReadFile("testdata/encrypted-extends-base.yaml")
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
			basePath := filepath.Join(dir, "base.yaml")
			writeTestFiles(t, dir, map[string]string{
				"base.yaml":    string(tt.content),
				"compose.yaml": fmt.Sprintf(extendsComposeFile, "base.yaml"),
			})

			_, err := LoadCompose(t.Context(), nil, dir, dir, "encrypted-extends", []string{filepath.Join(dir, "compose.yaml")},
				nil, nil, map[string]string{}, ComposeLoadOptions{})
			if !errors.Is(err, ErrComposeFileEncrypted) {
				t.Fatalf("LoadCompose() error = %v, want ErrComposeFileEncrypted", err)
			}

			msg := err.Error()
			for _, want := range []string{basePath, tt.wantReason, "extends of service app"} {
				if !strings.Contains(msg, want) {
					t.Errorf("LoadCompose() error = %q, want it to contain %q", msg, want)
				}
			}

			got, readErr := os.ReadFile(basePath) // #nosec G304: path is in the test temp directory.
			if readErr != nil {
				t.Fatal(readErr)
			}

			if !bytes.Equal(got, tt.content) {
				t.Error("LoadCompose() changed the encrypted extends file")
			}
		})
	}
}

func TestLoadCompose_EncryptedExtendsFileLayouts(t *testing.T) {
	child := os.Getenv(encryptedIncludeTestChild) == "1"
	if !child {
		t.Parallel()
	}

	fixture, err := os.ReadFile("testdata/encrypted-extends-base.yaml")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		files     map[string]string
		encrypted string
		service   string
	}{
		{
			name: "extends chain",
			files: map[string]string{
				"compose.yaml":       fmt.Sprintf(extendsComposeFile, "middle/middle.yaml"),
				"middle/middle.yaml": "services:\n  base:\n    extends:\n      file: ../secret/base.yaml\n      service: base\n",
				"secret/base.yaml":   string(fixture),
			},
			encrypted: "secret/base.yaml",
			service:   "app",
		},
		{
			name: "extends in included file",
			files: map[string]string{
				"compose.yaml":          "include:\n  - included/compose.yaml\n",
				"included/compose.yaml": fmt.Sprintf(extendsComposeFile, "base.yaml"),
				"included/base.yaml":    string(fixture),
			},
			encrypted: "included/base.yaml",
			service:   "app",
		},
		{
			name: "extends in same file",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    extends: middle\n" +
					"  middle:\n    extends:\n      file: base.yaml\n      service: base\n",
				"base.yaml": string(fixture),
			},
			encrypted: "base.yaml",
			service:   "middle",
		},
		{
			name: "include with project directory",
			files: map[string]string{
				"compose.yaml":     "include:\n  - path: sub/compose.yaml\n    project_directory: other\n",
				"sub/compose.yaml": fmt.Sprintf(extendsComposeFile, "base.yaml"),
				"other/base.yaml":  string(fixture),
			},
			encrypted: "other/base.yaml",
			service:   "app",
		},
		{
			// The service other in middle.yaml extends the encrypted file, but app extends only base.
			// The literal SOPS value in app starts the check, which must not report the file.
			name: "encrypted file extended by another service",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    extends:\n      file: middle.yaml\n      service: base\n" +
					"    environment:\n      FOO: \"ENC[AES256_GCM,data:abc]\"\n",
				"middle.yaml": "services:\n  base:\n    image: alpine\n" +
					"  other:\n    extends:\n      file: base.yaml\n      service: base\n",
				"base.yaml": string(fixture),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !child {
				runEncryptedIncludeTest(t)

				return
			}

			dir := t.TempDir()
			writeTestFiles(t, dir, tt.files)

			_, err := LoadCompose(t.Context(), nil, dir, dir, "encrypted-extends", []string{filepath.Join(dir, "compose.yaml")},
				nil, nil, map[string]string{}, ComposeLoadOptions{})
			if tt.encrypted == "" {
				if err != nil {
					t.Fatalf("LoadCompose() error = %v, want nil", err)
				}

				return
			}

			if !errors.Is(err, ErrComposeFileEncrypted) {
				t.Fatalf("LoadCompose() error = %v, want ErrComposeFileEncrypted", err)
			}

			msg := err.Error()
			for _, want := range []string{filepath.Join(dir, tt.encrypted), "no SOPS key is configured", "extends of service " + tt.service} {
				if !strings.Contains(msg, want) {
					t.Errorf("LoadCompose() error = %q, want it to contain %q", msg, want)
				}
			}
		})
	}
}

func TestFindEncryptedExtendsFile(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile("testdata/encrypted-extends-base.yaml")
	if err != nil {
		t.Fatal(err)
	}

	const extendsBaseFile = "services:\n  base:\n    extends:\n      file: %s\n      service: base\n"

	dir := t.TempDir()
	writeTestFiles(t, dir, map[string]string{
		"plain.yaml":   "services:\n  base:\n    image: alpine\n",
		"cycle-a.yaml": fmt.Sprintf(extendsBaseFile, "cycle-b.yaml"),
		"cycle-b.yaml": fmt.Sprintf(extendsBaseFile, "cycle-a.yaml"),
		"remote.yaml":  fmt.Sprintf(extendsComposeFile, "oci://example.com/compose:latest"),
		"chain.yaml":   fmt.Sprintf(extendsComposeFile, "sub/encrypted.yaml"),
		"same.yaml": "services:\n  app:\n    extends: middle\n" +
			"  middle:\n    extends:\n      service: base\n" +
			"  base:\n    extends:\n      file: sub/encrypted.yaml\n      service: base\n",
		"unrelated.yaml": "services:\n  base:\n    image: alpine\n" +
			"  other:\n    extends:\n      file: sub/encrypted.yaml\n      service: base\n",
		"sub/encrypted.yaml": string(fixture),
	})

	tests := []struct {
		file    string
		service string
		want    string
	}{
		{file: "plain.yaml", service: "base"},
		{file: "cycle-a.yaml", service: "base"},
		{file: "remote.yaml", service: "app"},
		{file: "missing.yaml", service: "base"},
		{file: "chain.yaml", service: "app", want: "sub/encrypted.yaml"},
		{file: "chain.yaml", service: "missing"},
		{file: "same.yaml", service: "app", want: "sub/encrypted.yaml"},
		{file: "unrelated.yaml", service: "base"},
		{file: "unrelated.yaml", service: "other", want: "sub/encrypted.yaml"},
		{file: "sub/encrypted.yaml", service: "base", want: "sub/encrypted.yaml"},
	}

	for _, tt := range tests {
		t.Run(tt.file+"/"+tt.service, func(t *testing.T) {
			t.Parallel()

			want := ""
			if tt.want != "" {
				want = filepath.Join(dir, tt.want)
			}

			if got := findEncryptedExtendsFile(filepath.Join(dir, tt.file), tt.service, map[string]bool{}); got != want {
				t.Fatalf("findEncryptedExtendsFile() = %q, want %q", got, want)
			}
		})
	}
}

// TestLoadCompose_EncryptedExtendsFileAfterPublish publishes a repository and loads the
// published project, as a deployment does. Publish decrypts the extends file in the
// repository root, and skips the ones in an excluded directory and matching .gitignore.
func TestLoadCompose_EncryptedExtendsFileAfterPublish(t *testing.T) {
	child := os.Getenv(encryptedIncludeTestChild) == "1"
	if !child {
		t.Parallel()
	}

	fixture, err := os.ReadFile("testdata/encrypted-extends-base.yaml")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		base      string
		gitignore string
		wantErr   bool
	}{
		{name: "decrypted by publish", base: "base.yaml"},
		{name: "excluded directory", base: ".github/base.yaml", wantErr: true},
		{name: "matches gitignore", base: "secret/base.yaml", gitignore: "secret/\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !child {
				runEncryptedIncludeTest(t)

				return
			}

			encryption.SetupAgeKeyEnvVar(t)

			source := t.TempDir()
			files := map[string]string{
				"compose.yaml": fmt.Sprintf(extendsComposeFile, tt.base),
				tt.base:        string(fixture),
			}

			if tt.gitignore != "" {
				files[".gitignore"] = tt.gitignore
			}

			commitTestRepository(t, source, files)

			s, err := store.NewGitStore(store.GitStoreOptions{CloneURL: "file://" + source, BaseDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}

			rev, err := s.Resolve(t.Context(), "main")
			if err != nil {
				t.Fatal(err)
			}

			artifact, err := s.Publish(t.Context(), rev)
			if err != nil {
				t.Fatal(err)
			}

			project, err := LoadCompose(t.Context(), nil, artifact.Path, artifact.Path, "encrypted-extends",
				[]string{filepath.Join(artifact.Path, "compose.yaml")}, nil, nil, map[string]string{}, ComposeLoadOptions{})

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("LoadCompose() error = %v", err)
				}

				if image := project.Services["app"].Image; image != "alpine" {
					t.Fatalf("service app image = %q, want %q", image, "alpine")
				}

				return
			}

			if !errors.Is(err, ErrComposeFileEncrypted) {
				t.Fatalf("LoadCompose() error = %v, want ErrComposeFileEncrypted", err)
			}

			msg := err.Error()
			for _, want := range []string{filepath.Join(artifact.Path, tt.base), "it was not decrypted when doco-cd fetched the source"} {
				if !strings.Contains(msg, want) {
					t.Errorf("LoadCompose() error = %q, want it to contain %q", msg, want)
				}
			}
		})
	}
}

// commitTestRepository creates a Git repository in dir with one commit on main that contains files.
func commitTestRepository(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}

	err = repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main")))
	if err != nil {
		t.Fatal(err)
	}

	writeTestFiles(t, dir, files)

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	for name := range files {
		if _, err = wt.Add(name); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}

	_, err = wt.Commit("add files", &gogit.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
}
