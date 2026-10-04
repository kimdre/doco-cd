package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/filesystem"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

// ArtifactsSubdir is the fixed directory name, relative to a store's base
// directory, that holds one subdirectory per published revision.
const ArtifactsSubdir = "artifacts"

// MirrorSubdir is the fixed directory name, relative to a store's base
// directory, that holds GitStore's underlying bare mirror clone.
// Exported so the startup migration pass (internal/migration) can recognize and
// lay out the same directory a fresh GitStore would use, without importing GitStore itself.
const MirrorSubdir = "mirror"

// SubmodulesSubdir is the fixed directory name for cached Git submodule mirrors.
const SubmodulesSubdir = "submodules"

// ComposeGitCacheSubdir holds Git include stores keyed by remote and reference.
const ComposeGitCacheSubdir = "compose-git-cache"

// LiveSubdir is the fixed directory name, relative to a store's base directory, that holds the
// mutable copies of deployed files that are updated in place instead of per revision
// (e.g. bind mounts that are excluded from recreation with recreate.ignore).
const LiveSubdir = "live"

// artifactPath returns the final directory a revision is published to, e.g. "<baseDir>/artifacts/<revision>".
func artifactPath(baseDir string, revision Revision) (string, error) {
	name := artifactDirName(revision)
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("%w: %q", ErrInvalidRevision, revision)
	}

	if name == "." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("%w: %q", ErrInvalidRevision, revision)
	}

	return filepath.Join(baseDir, ArtifactsSubdir, name), nil
}

// artifactDirName encodes revisions for use in Docker bind-mount paths. OCI digests use ":", which Docker treats as a
// volume separator, so it is replaced with "-"; revisionFromDirName reverses the encoding.
func artifactDirName(revision Revision) string {
	return strings.ReplaceAll(string(revision), ":", "-")
}

// revisionFromDirName reverses artifactDirName.
func revisionFromDirName(name string) Revision {
	for _, algorithm := range [...]string{"sha256", "sha384", "sha512"} {
		if digest, found := strings.CutPrefix(name, algorithm+"-"); found {
			return Revision(algorithm + ":" + digest)
		}
	}

	return Revision(name)
}

// ArtifactDirName exposes the revision-to-directory-name encoding applied by
// artifactPath, for callers that need to locate or lay out an artifact
// directory without going through a Store.
func ArtifactDirName(revision Revision) string {
	return artifactDirName(revision)
}

// ArtifactRoot returns the artifact directory ("<baseDir>/artifacts/<revision>") that path is nested under, and the
// revision it was published at, if any. It performs no filesystem access - it is a pure path computation for callers
// that only have a descendant path (e.g. a working directory recorded in a container label) and need to recover the
// artifact root/revision it was published under.
func ArtifactRoot(baseDir, path string) (root string, revision Revision, ok bool) {
	artifactsDir := filepath.Join(baseDir, ArtifactsSubdir)

	rel, err := filepath.Rel(artifactsDir, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", false
	}

	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	if first == "" {
		return "", "", false
	}

	return filepath.Join(artifactsDir, first), revisionFromDirName(first), true
}

// lookupArtifact reports whether a revision has already been published
// under baseDir, without materializing anything.
func lookupArtifact(baseDir string, revision Revision) (Artifact, bool, error) {
	path, err := artifactPath(baseDir, revision)
	if err != nil {
		return Artifact{}, false, err
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Artifact{}, false, nil
		}

		return Artifact{}, false, fmt.Errorf("stat artifact %s: %w", revision, err)
	}

	if !info.IsDir() {
		return Artifact{}, false, fmt.Errorf("artifact path %s is not a directory", path)
	}

	published, err := isPublished(path)
	if err != nil {
		return Artifact{}, false, fmt.Errorf("verify artifact %s: %w", revision, err)
	}

	if !published {
		return Artifact{}, false, nil
	}

	return Artifact{Revision: revision, Path: path}, true, nil
}

// publishedSuffix names the file next to an artifact directory that records the identity (see filesystem.Identity) of
// the directory publishDir published there.
//
// A directory at an artifact's path is not necessarily the published artifact: when a container starts while one of
// its bind-mount sources is missing (e.g. because the artifact was deleted), Docker re-creates the source as an empty
// directory, and with it the artifact's path. Treating such a directory as published would deploy every later stack
// onto empty bind mounts.
const publishedSuffix = ".published"

// publishLockSuffix names the path lock (see sourcecache.AcquireRequiredExclusivePathLock) publishDir holds while it
// moves an artifact into place. Its lock file is the artifact's path followed by publishLockSuffix and ".lock".
const publishLockSuffix = ".publish"

// isPublished reports whether the directory at path is the artifact publishDir published there. A directory without a
// record - published before records were written, or by a publish interrupted right after its rename - is accepted
// and recorded if it contains anything but directories, which a re-created bind-mount source does not.
func isPublished(path string) (bool, error) {
	identity, err := filesystem.Identity(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}

		return false, err
	}

	recorded, err := os.ReadFile(path + publishedSuffix)
	if err == nil {
		return string(recorded) == identity, nil
	}

	if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}

	hasFiles, err := containsNonDirectory(path)
	if err != nil || !hasFiles {
		return false, err
	}

	// Recording is only an optimization here: without a record, the next lookup decides the same way.
	_ = recordPublished(path)

	return true, nil
}

// recordPublished records the identity of the artifact directory at path, see publishedSuffix.
func recordPublished(path string) error {
	identity, err := filesystem.Identity(path)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), tempArtifactPrefix+filepath.Base(path)+publishedSuffix+"-*")
	if err != nil {
		return err
	}

	_, err = tmp.WriteString(identity)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}

	if err == nil {
		err = os.Rename(tmp.Name(), path+publishedSuffix)
	}

	if err != nil {
		_ = os.Remove(tmp.Name())
	}

	return err
}

// containsNonDirectory reports whether the tree at root contains anything but directories.
func containsNonDirectory(root string) (bool, error) {
	found := false

	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			found = true
			return filepath.SkipAll
		}

		return nil
	})

	return found, err
}

// setAsideUnpublished moves a directory at path that is not the published artifact (see isPublished) out of the way,
// to a temporary name that sweepOrphanedTemp removes once it was left alone for orphanedTempMaxAge: running
// containers may still have it mounted until they are recreated.
func setAsideUnpublished(artifactsDir, path string) error {
	aside := filepath.Join(artifactsDir,
		tempArtifactPrefix+"unpublished-"+filepath.Base(path)+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))

	if err := os.Rename(path, aside); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return err
	}

	now := time.Now()

	return os.Chtimes(aside, now, now)
}

// listArtifacts returns every already-published artifact under baseDir.
func listArtifacts(baseDir string) ([]Artifact, error) {
	entries, err := os.ReadDir(filepath.Join(baseDir, ArtifactsSubdir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("list artifacts: %w", err)
	}

	artifacts := make([]Artifact, 0, len(entries))

	for _, e := range entries {
		// Temporary directories created by an in-progress publishDir call
		// are not artifacts yet - they are renamed into place only once
		// fully written. Without this check, List() (and anything built on
		// it, e.g. garbage collection) could observe and act on another
		// caller's in-flight publish.
		if !e.IsDir() || strings.HasPrefix(e.Name(), tempArtifactPrefix) {
			continue
		}

		artifacts = append(artifacts, Artifact{
			Revision: revisionFromDirName(e.Name()),
			Path:     filepath.Join(baseDir, ArtifactsSubdir, e.Name()),
		})
	}

	return artifacts, nil
}

// ListArtifacts returns every artifact already published under baseDir,
// without requiring a fully constructed Store. GitStore.List and
// OCIStore.List are both thin wrappers over this same function; it is
// exported directly for callers (e.g. garbage collection, see internal/gc)
// that need to inspect a base directory without holding the per-source-type
// options (remote URL, credentials, ...) a real Store requires.
func ListArtifacts(baseDir string) ([]Artifact, error) {
	return listArtifacts(baseDir)
}

// tempArtifactPrefix is the prefix publishDir gives the temporary directory
// it builds an artifact in before renaming it into place.
const tempArtifactPrefix = ".tmp-"

// orphanedTempMaxAge is how long a temporary artifact directory has to be
// untouched before sweepOrphanedTemp treats it as abandoned. It only needs
// to exceed the time a single publish can plausibly take (a clone plus a
// tree export), since the cost of guessing too low is destroying a healthy
// in-flight publish, while guessing too high only delays reclaiming disk.
const orphanedTempMaxAge = time.Hour

// publishDir atomically materializes an artifact directory: it calls write
// to populate a temporary directory next to the final destination, then
// renames it into place. If another caller wins the race and publishes the
// same revision first, the temporary directory is discarded and the
// existing artifact is returned - this is what makes concurrent Publish
// calls for the same revision safe. A directory at the destination that is
// not the published artifact (see publishedSuffix) is moved aside first.
func publishDir(baseDir string, revision Revision, write func(dir string) error) (Artifact, error) {
	finalPath, err := artifactPath(baseDir, revision)
	if err != nil {
		return Artifact{}, err
	}

	artifactsDir := filepath.Join(baseDir, ArtifactsSubdir)
	if err := os.MkdirAll(artifactsDir, filesystem.PermDir); err != nil {
		return Artifact{}, fmt.Errorf("create artifacts directory: %w", err)
	}

	tmpDir, err := os.MkdirTemp(artifactsDir, tempArtifactPrefix+"*")
	if err != nil {
		return Artifact{}, fmt.Errorf("create temporary artifact directory: %w", err)
	}

	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err := write(tmpDir); err != nil {
		return Artifact{}, err
	}

	// os.MkdirTemp always creates tmpDir with mode 0700, regardless of
	// filesystem.PermDir; without this, the published artifact root - bind
	// mounted directly into deployed containers - is unreadable by any
	// user other than its owner.
	if err := os.Chmod(tmpDir, filesystem.PermDir); err != nil {
		return Artifact{}, fmt.Errorf("set permissions on artifact directory: %w", err)
	}

	// Under this lock, a directory at finalPath that is not accepted as the published artifact was not just renamed
	// there by a concurrent publish, but e.g. re-created by Docker, and can be moved aside.
	unlock, err := sourcecache.AcquireRequiredExclusivePathLock(finalPath + publishLockSuffix)
	if err != nil {
		return Artifact{}, fmt.Errorf("lock publish of artifact %s: %w", revision, err)
	}
	defer unlock()

	if existing, ok, err := lookupArtifact(baseDir, revision); err != nil {
		return Artifact{}, err
	} else if ok {
		return existing, nil
	}

	if err := setAsideUnpublished(artifactsDir, finalPath); err != nil {
		return Artifact{}, fmt.Errorf("set aside unpublished directory of artifact %s: %w", revision, err)
	}

	if err := os.Rename(tmpDir, finalPath); err != nil {
		// Another caller may have published the same revision concurrently;
		// its content is identical (revisions are immutable), so prefer it
		// over failing.
		if existing, ok, lookupErr := lookupArtifact(baseDir, revision); lookupErr == nil && ok {
			return existing, nil
		}

		return Artifact{}, fmt.Errorf("publish artifact %s: %w", revision, err)
	}

	if err := recordPublished(finalPath); err != nil {
		return Artifact{}, fmt.Errorf("record published artifact %s: %w", revision, err)
	}

	return Artifact{Revision: revision, Path: finalPath}, nil
}

// sweepOrphanedTemp removes temporary artifact directories and files left
// behind by a process that was killed mid-publish, as well as directories
// setAsideUnpublished moved out of the way.
//
// A store is constructed per deployment, not once per process, and several
// of them - in this process or in another doco-cd instance sharing the same
// data volume - can be publishing different revisions of the same repository
// at the same time. Removing every temporary directory unconditionally would
// therefore delete a healthy, in-flight publish out from under its writer,
// which then fails mid-export with a confusing ENOENT. Only directories that
// have been untouched for orphanedTempMaxAge are swept, since no live
// publish leaves its temporary directory idle that long.
func sweepOrphanedTemp(baseDir string) error {
	artifactsDir := filepath.Join(baseDir, ArtifactsSubdir)

	entries, err := os.ReadDir(artifactsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("sweep orphaned temp: read %s: %w", artifactsDir, err)
	}

	var errs []error

	cutoff := time.Now().Add(-orphanedTempMaxAge)

	for _, e := range entries {
		// Besides directories, this also removes temporary files recordPublished failed to rename into place.
		if !strings.HasPrefix(e.Name(), tempArtifactPrefix) {
			continue
		}

		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				// Swept by a concurrent store in the meantime.
				continue
			}

			errs = append(errs, fmt.Errorf("sweep orphaned temp: stat %s: %w", e.Name(), err))

			continue
		}

		if info.ModTime().After(cutoff) {
			continue
		}

		if err := os.RemoveAll(filepath.Join(artifactsDir, e.Name())); err != nil {
			errs = append(errs, fmt.Errorf("sweep orphaned temp: remove %s: %w", e.Name(), err))
		}
	}

	return errors.Join(errs...)
}
