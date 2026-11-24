package docker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

// ErrComposeFileEncrypted means that compose-go loaded a compose file that is still SOPS-encrypted.
var ErrComposeFileEncrypted = errors.New("compose file is still SOPS-encrypted")

// explainEncryptedComposeFile adds SOPS diagnostics to a Compose validation error for an encrypted file.
func explainEncryptedComposeFile(err error) error {
	file := composeValidationFile(err)
	if file == "" || !filesystem.IsFile(file) {
		return err
	}

	content, readErr := os.ReadFile(file) // #nosec G304: path is the regular Compose file named in the validation error.
	if readErr != nil {
		return err
	}

	format, encrypted := encryption.DetectFormat(content, file)
	if !encrypted {
		return err
	}

	reason := "no SOPS key is configured"

	if encryption.SopsKeyIsSet() {
		// Decrypt the same bytes in memory. Do not modify the source file.
		_, decryptErr := encryption.DecryptContent(content, format)

		switch {
		case decryptErr == nil:
			ignored := filesystem.IgnoreDirs.ToSlice()
			slices.Sort(ignored)

			reason = fmt.Sprintf("it was not decrypted when doco-cd fetched the source; "+
				"check whether the file is outside the source tree, matches .gitignore, or is in an excluded directory (%s)",
				strings.Join(ignored, ", "))
		case encryption.IsKeyUnavailable(decryptErr):
			reason = fmt.Sprintf("SOPS could not retrieve the data key; check your key or key service access: %v", decryptErr)
		default:
			reason = fmt.Sprintf("decryption failed: %v", decryptErr)
		}
	}

	return fmt.Errorf("%w: %s: %s (original error: %w)", ErrComposeFileEncrypted, file, reason, err)
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
