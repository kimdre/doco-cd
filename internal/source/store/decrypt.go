package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

// decryptRecordSuffix names the sibling file of an artifact directory that lists
// the files decrypted in it, keyed by ciphertext. GC removes it with the artifact.
const decryptRecordSuffix = ".decrypted.json"

var errPlaintextStale = errors.New("plaintext changed since it was recorded")

// decryptRecord lists the SOPS files decrypted in one artifact, keyed by their
// slash-separated path relative to the artifact root.
type decryptRecord struct {
	Files map[string]decryptedFile `json:"files"`
}

// decryptedFile identifies a decrypted file by its ciphertext and pins the
// plaintext as written, so a later publish can tell a tampered copy apart.
type decryptedFile struct {
	Ciphertext string `json:"ciphertext"` // sha256 of the encrypted content
	Size       int64  `json:"size"`
	ModTime    int64  `json:"mod_time"` // unix nanoseconds
}

// plaintextSource is a decrypted file of an already published artifact.
type plaintextSource struct {
	path    string
	size    int64
	modTime int64
}

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
// A file whose ciphertext is identical to one decrypted in an earlier artifact
// under baseDir takes that artifact's plaintext instead of a key service call.
// Most revisions change no secret, so this keeps the per-revision cost of a
// cloud KMS near zero. The plaintext is only reused when its size and mtime
// still match the record, anything else is decrypted again.
//
// Files a compose project reaches through a bind mount outside the artifact
// tree (an arbitrary host path) are outside this scope; they were never
// part of the source revision this artifact represents.
func decryptArtifact(log *slog.Logger, baseDir, dir string) (decryptRecord, error) {
	sources := loadPlaintextSources(log, baseDir)
	record := decryptRecord{Files: map[string]decryptedFile{}}

	var reused, decrypted int

	decryptFile := func(path string) (bool, error) {
		content, err := os.ReadFile(path) // #nosec G304 -- path comes from walking the artifact directory
		if err != nil {
			return false, fmt.Errorf("failed to read file %s: %w", path, err)
		}

		if _, isEncrypted := encryption.DetectFormat(content, path); !isEncrypted {
			return false, nil
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return false, fmt.Errorf("failed to resolve %s relative to %s: %w", path, dir, err)
		}

		sum := sha256.Sum256(content)
		ciphertext := hex.EncodeToString(sum[:])

		copied := false

		if src, ok := sources[ciphertext]; ok {
			if err = copyPlaintext(src, path); err == nil {
				copied = true
			} else {
				log.Debug("not reusing plaintext of earlier artifact", slog.String("path", path), slog.Any("error", err))
			}
		}

		switch {
		case copied:
			reused++
		default:
			if _, err = encryption.DecryptToFile(path, content); err != nil {
				return false, err
			}

			decrypted++
		}

		info, err := os.Stat(path)
		if err != nil {
			return false, fmt.Errorf("failed to stat decrypted file %s: %w", path, err)
		}

		record.Files[filepath.ToSlash(rel)] = decryptedFile{Ciphertext: ciphertext, Size: info.Size(), ModTime: info.ModTime().UnixNano()}

		return true, nil
	}

	_, err := encryption.DecryptFilesInDirectoryTolerant(dir, dir, decryptFile, func(path string, err error) {
		log.Warn("skipping file that could not be decrypted",
			slog.String("path", path), slog.Any("error", err))
	})

	if reused > 0 || decrypted > 0 {
		log.Debug("decrypted artifact files", slog.Int("decrypted", decrypted), slog.Int("reused", reused))
	}

	return record, err
}

// loadPlaintextSources indexes the decrypted files of every published artifact
// under baseDir by ciphertext. The newest artifact wins on identical ciphertext.
func loadPlaintextSources(log *slog.Logger, baseDir string) map[string]plaintextSource {
	sources := map[string]plaintextSource{}

	artifacts, err := listArtifacts(baseDir)
	if err != nil {
		log.Debug("failed to list artifacts for plaintext reuse", slog.Any("error", err))

		return sources
	}

	type recorded struct {
		Artifact
		modTime int64
		record  decryptRecord
	}

	var records []recorded

	for _, a := range artifacts {
		data, err := os.ReadFile(a.Path + decryptRecordSuffix) // #nosec G304 -- path derived from the store's own artifact listing
		if err != nil {
			continue
		}

		var r decryptRecord
		if err = json.Unmarshal(data, &r); err != nil {
			log.Debug("ignoring unreadable decrypt record", slog.String("artifact", string(a.Revision)), slog.Any("error", err))

			continue
		}

		info, err := os.Stat(a.Path)
		if err != nil {
			continue
		}

		records = append(records, recorded{Artifact: a, modTime: info.ModTime().UnixNano(), record: r})
	}

	sort.Slice(records, func(i, j int) bool { return records[i].modTime < records[j].modTime })

	for _, r := range records {
		for rel, f := range r.record.Files {
			sources[f.Ciphertext] = plaintextSource{path: filepath.Join(r.Path, filepath.FromSlash(rel)), size: f.Size, modTime: f.ModTime}
		}
	}

	return sources
}

// copyPlaintext writes src's content to dst, unless src no longer matches its record.
func copyPlaintext(src plaintextSource, dst string) error {
	info, err := os.Stat(src.path)
	if err != nil {
		return err
	}

	if info.Size() != src.size || info.ModTime().UnixNano() != src.modTime {
		return fmt.Errorf("%w: %s", errPlaintextStale, src.path)
	}

	content, err := os.ReadFile(src.path) // #nosec G304 -- path comes from the store's own decrypt record
	if err != nil {
		return err
	}

	// #nosec G703 -- dst is a file inside the artifact directory being published.
	return os.WriteFile(dst, content, filesystem.PermOwner)
}

// writeDecryptRecord stores record next to the published artifact. A failure
// only costs reuse on the next publish, so it is logged, not returned.
func writeDecryptRecord(log *slog.Logger, artifact Artifact, record decryptRecord) {
	if len(record.Files) == 0 {
		return
	}

	data, err := json.Marshal(record)
	if err == nil {
		// #nosec G703 -- sibling of the artifact directory, inside the store's base directory.
		err = os.WriteFile(artifact.Path+decryptRecordSuffix, data, filesystem.PermOwner)
	}

	if err != nil {
		log.Warn("failed to write decrypt record", slog.String("artifact", string(artifact.Revision)), slog.Any("error", err))
	}
}
