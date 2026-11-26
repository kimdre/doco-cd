package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

// ErrComposeFileEncrypted means that compose-go loaded a compose file that is still SOPS-encrypted.
var ErrComposeFileEncrypted = errors.New("compose file is still SOPS-encrypted")

// sopsValuePrefix starts every value that SOPS encrypted.
var sopsValuePrefix = []byte("ENC[AES256_GCM,")

// explainEncryptedComposeFile adds SOPS diagnostics to a Compose validation error for an encrypted file.
func explainEncryptedComposeFile(err error) error {
	file := composeValidationFile(err)
	if file == "" {
		return err
	}

	reason, encrypted := encryptedFileReason(file)
	if !encrypted {
		return err
	}

	return fmt.Errorf("%w: %s: %s (original error: %w)", ErrComposeFileEncrypted, file, reason, err)
}

// encryptedFileReason reports whether the regular file is still SOPS-encrypted, and why it was not decrypted.
func encryptedFileReason(file string) (string, bool) {
	if !filesystem.IsFile(file) {
		return "", false
	}

	content, err := os.ReadFile(file) // #nosec G304: path is a regular Compose file named by Compose or by an extends entry.
	if err != nil {
		return "", false
	}

	format, encrypted := encryption.DetectFormat(content, file)
	if !encrypted {
		return "", false
	}

	if !encryption.SopsKeyIsSet() {
		return "no SOPS key is configured", true
	}

	// Decrypt the same bytes in memory. Do not modify the source file.
	_, decryptErr := encryption.DecryptContent(content, format)

	switch {
	case decryptErr == nil:
		ignored := filesystem.IgnoreDirs.ToSlice()
		slices.Sort(ignored)

		return fmt.Sprintf("it was not decrypted when doco-cd fetched the source; "+
			"check whether the file is outside the source tree, matches .gitignore, or is in an excluded directory (%s)",
			strings.Join(ignored, ", ")), true
	case encryption.IsKeyUnavailable(decryptErr):
		return fmt.Sprintf("SOPS could not retrieve the data key; check your key or key service access: %v", decryptErr), true
	default:
		return fmt.Sprintf("decryption failed: %v", decryptErr), true
	}
}

// composeValidationFile uses the wrapped cause to find the end of the path.
// A Compose validation error has the form "validating <path>: <cause>".
func composeValidationFile(err error) string {
	for ; err != nil; err = errors.Unwrap(err) {
		msg, ok := strings.CutPrefix(err.Error(), "validating ")
		if !ok {
			continue
		}

		cause := errors.Unwrap(err)
		if cause == nil {
			return ""
		}

		if file, ok := strings.CutSuffix(msg, ": "+cause.Error()); ok && filepath.IsAbs(file) {
			return file
		}
	}

	return ""
}

// checkEncryptedExtendsFiles returns an error when a service of project contains SOPS-encrypted values
// and a local file reached through `extends: file:` is still SOPS-encrypted.
//
// compose-go reads only the services section of an extends file and does not validate it,
// so an encrypted extends file loads without an error and its encrypted values become the service configuration.
func checkEncryptedExtendsFiles(ctx context.Context, options *cli.ProjectOptions, project *types.Project) error {
	services, err := json.Marshal(project.Services)
	if err != nil {
		return fmt.Errorf("failed to check services for SOPS-encrypted values: %w", err)
	}

	if !bytes.Contains(services, sopsValuePrefix) {
		return nil
	}

	return findEncryptedExtendsInProject(ctx, options, nil)
}

// explainEncryptedLoadError adds SOPS diagnostics to a Compose load error.
// SOPS stores typed values (int, bool, ports) as strings. compose-go cannot cast them,
// so the load fails before the project exists. The error then names no file.
func explainEncryptedLoadError(ctx context.Context, options *cli.ProjectOptions, err error) error {
	explained := explainEncryptedComposeFile(err)
	if errors.Is(explained, ErrComposeFileEncrypted) || !strings.Contains(err.Error(), string(sopsValuePrefix)) {
		return explained
	}

	if extendsErr := findEncryptedExtendsInProject(ctx, options, err); errors.Is(extendsErr, ErrComposeFileEncrypted) {
		return extendsErr
	}

	return explained
}

// findEncryptedExtendsInProject loads the project without extends and follows each local extends entry.
// It returns ErrComposeFileEncrypted for the first SOPS-encrypted file. cause is the load error to wrap, or nil.
func findEncryptedExtendsInProject(ctx context.Context, options *cli.ProjectOptions, cause error) error {
	// Load the project again without applying extends, so that each service keeps its
	// extends entry with the path resolved by compose-go, also in included files.
	// The copy keeps the load options of the first load unchanged.
	extendsOptions := *options

	err := cli.WithLoadOptions(func(o *loader.Options) {
		o.SkipExtends = true
		o.SkipConsistencyCheck = true
	})(&extendsOptions)
	if err != nil {
		return fmt.Errorf("failed to set load options to check extends files: %w", err)
	}

	unextended, err := extendsOptions.LoadProject(ctx)
	if err != nil {
		return fmt.Errorf("failed to load compose project to check extends files: %w", err)
	}

	visited := map[string]bool{}

	for _, name := range slices.Sorted(maps.Keys(unextended.Services)) {
		extends := unextended.Services[name].Extends
		if extends == nil || extends.File == "" || !filepath.IsAbs(extends.File) {
			continue
		}

		file := findEncryptedExtendsFile(extends.File, extends.Service, visited)
		if file == "" {
			continue
		}

		reason, _ := encryptedFileReason(file)

		if cause != nil {
			return fmt.Errorf("%w: %s: %s (reached through extends of service %s, original error: %w)",
				ErrComposeFileEncrypted, file, reason, name, cause)
		}

		return fmt.Errorf("%w: %s: %s (reached through extends of service %s)", ErrComposeFileEncrypted, file, reason, name)
	}

	return nil
}

// findEncryptedExtendsFile returns file if it is still SOPS-encrypted. Otherwise, it follows the extends
// entry of service in file. compose-go resolves a relative `extends: file:` path relative to the directory of file.
// The visited keys are file and service, separated by a NUL byte.
func findEncryptedExtendsFile(file, service string, visited map[string]bool) string {
	key := file + "\x00" + service
	if visited[key] || !filesystem.IsFile(file) {
		return ""
	}

	visited[key] = true

	content, err := os.ReadFile(file) // #nosec G304: path is a regular file named by an extends entry.
	if err != nil {
		return ""
	}

	if _, encrypted := encryption.DetectFormat(content, file); encrypted {
		return file
	}

	var model struct {
		Services map[string]struct {
			Extends any `yaml:"extends"`
		} `yaml:"services"`
	}

	if err = yaml.Unmarshal(content, &model); err != nil {
		return ""
	}

	switch extends := model.Services[service].Extends.(type) {
	case string:
		return findEncryptedExtendsFile(file, extends, visited)
	case map[string]any:
		next, ok := extends["service"].(string)
		if !ok || next == "" {
			return ""
		}

		nextFile := file

		if f, ok := extends["file"].(string); ok && f != "" {
			nextFile = f
			if !filepath.IsAbs(nextFile) {
				nextFile = filepath.Join(filepath.Dir(file), nextFile)
			}
		}

		return findEncryptedExtendsFile(nextFile, next, visited)
	default:
		return ""
	}
}
