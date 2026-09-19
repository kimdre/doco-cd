package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/filesystem"
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

// artifactPath returns the final directory a revision is published to, e.g. "<baseDir>/artifacts/<revision>".
func artifactPath(baseDir string, revision Revision) string {
	return filepath.Join(baseDir, ArtifactsSubdir, artifactDirName(revision))
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
	path := artifactPath(baseDir, revision)

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

	return Artifact{Revision: revision, Path: path}, true, nil
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
// calls for the same revision safe.
func publishDir(baseDir string, revision Revision, write func(dir string) error) (Artifact, error) {
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

	finalPath := artifactPath(baseDir, revision)

	if err := os.Rename(tmpDir, finalPath); err != nil {
		// Another caller may have published the same revision concurrently;
		// its content is identical (revisions are immutable), so prefer it
		// over failing.
		if existing, ok, lookupErr := lookupArtifact(baseDir, revision); lookupErr == nil && ok {
			return existing, nil
		}

		return Artifact{}, fmt.Errorf("publish artifact %s: %w", revision, err)
	}

	return Artifact{Revision: revision, Path: finalPath}, nil
}

// sweepOrphanedTemp removes temporary artifact directories left behind by a
// process that was killed mid-publish.
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
		if !e.IsDir() || !strings.HasPrefix(e.Name(), tempArtifactPrefix) {
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
