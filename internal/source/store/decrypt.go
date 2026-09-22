package store

import (
	"log/slog"

	"github.com/kimdre/doco-cd/internal/encryption"
)

// decryptArtifact walks dir - a freshly materialized, not-yet-published
// artifact directory - and decrypts every SOPS-encrypted file it can in-place,
// before the artifact is published (atomically renamed into its final, read-only location).
//
// Unlike the old/pre-artifact-store flow, this does not need to know which
// files a parsed compose project ends up referencing (bind mounts, build
// contexts, included env files, ...): the whole exported tree is decrypted
// up front, deterministically and independent of parse order. It also does
// not need a reset/manifest step to protect the plaintext from a later
// sync, since a published artifact is immutable and never synced back into.
//
// Decrypting the whole tree also means decrypting files no deployment will
// ever read, including ones encrypted for a recipient this instance is not.
// Those are logged and left as ciphertext rather than failing the publish,
// which would take down every stack in the repository over a file none of
// them use. A stack that does consume such a file still fails, with a
// precise error, when LoadCompose reaches it.
//
// Files a compose project reaches through a bind mount outside the artifact
// tree (an arbitrary host path) are outside this scope; they were never
// part of the source revision this artifact represents.
func decryptArtifact(log *slog.Logger, dir string) error {
	_, err := encryption.DecryptFilesInDirectoryTolerant(dir, dir, func(path string, err error) {
		log.Warn("skipping file that could not be decrypted",
			slog.String("path", path), slog.Any("error", err))
	})

	return err
}
